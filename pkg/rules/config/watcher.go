package config

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
)

const debounce = 400 * time.Millisecond

// Watcher watches a directory of YAML sidecar files and rebuilds the Registry
// when any file changes.
type Watcher struct {
	svcctx.ServiceContext
	dir     string
	current atomic.Pointer[Registry]
}

// Creates a Watcher for dir and does an initial load.
func NewWatcher(dir string) (*Watcher, error) {
	sc := svcctx.New("config-watcher")
	sc.Logger = logger.New(sc.Name(), "dev")

	w := &Watcher{ServiceContext: sc, dir: dir}

	errs := Validate(dir)
	for _, err := range errs {
		w.ErrorF("config validation: %v", err)
	}

	reg, err := NewRegistry(dir)
	if err != nil && reg == nil {
		return nil, err
	}
	if err != nil {
		w.ErrorF("initial load errors: %v", err)
	}
	w.current.Store(reg)
	return w, nil
}

// HasBlockingError reports whether pluginID currently has any blocking validation error.
// It runs Validate() fresh on every call so that IsReady() in the rule adapter always
// sees the current disk state - avoiding the race between the config watcher's reload
// debounce and the manager's reconcile firing from the same fsnotify event.
func (w *Watcher) HasBlockingError(pluginID string) bool {
	if pluginID == "" {
		return false
	}
	for _, err := range Validate(w.dir) {
		if err.Blocking() && err.PluginID == pluginID {
			return true
		}
	}
	return false
}

// HasBlockingErrorFor is like HasBlockingError but also matches by YAML file name
// (e.g. "brute_force.yaml"). This catches rules whose id: field is missing - those
// errors carry no PluginID, but do carry File set to the YAML filename.
func (w *Watcher) HasBlockingErrorFor(pluginID, yamlFile string) bool {
	for _, e := range Validate(w.dir) {
		if !e.Blocking() {
			continue
		}
		if pluginID != "" && e.PluginID == pluginID {
			return true
		}
		if yamlFile != "" && e.File == yamlFile {
			return true
		}
	}
	return false
}

// Returns the most recently loaded Registry.
func (w *Watcher) Current() *Registry {
	return w.current.Load()
}

// Starts the fsnotify watch loop. Blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) errors.Error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.NewE(err)
	}
	defer fsw.Close()

	if err := fsw.Add(w.dir); err != nil {
		return errors.NewE(err)
	}

	var timer *time.Timer
	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(debounce, w.reload)
	}

	for {
		select {
		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if isYAML(event.Name) {
				resetTimer()
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.ErrorF("fsnotify error: %v", err)
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil
		}
	}
}

func (w *Watcher) reload() {
	errs := Validate(w.dir)
	for _, err := range errs {
		w.ErrorF("config validation: %v", err)
	}

	reg, err := NewRegistry(w.dir)
	if err != nil {
		w.ErrorF("reload error: %v", err)
		if reg == nil {
			return
		}
	}
	w.current.Store(reg)
	w.Info("loaded %d rule configs from %s", reg.Len(), w.dir)
}

func isYAML(name string) bool {
	n := len(name)
	return (n > 5 && name[n-5:] == ".yaml") || (n > 4 && name[n-4:] == ".yml")
}

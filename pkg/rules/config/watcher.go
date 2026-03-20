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

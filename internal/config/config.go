// Package config provides the generic Registry and ConfigManager used by all
// plugin config packages (enrichments, formatters, matchers, tuning_rules).
//
// Each plugin type defines its own metadata struct and a Loader that provides
// Parse and optional Validate/CrossValidate hooks. No per-package loader boilerplate
// is needed beyond filling in the Loader struct fields.
package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/logger"
)

const debounce = 400 * time.Millisecond

// ConfigManager[T] is the generic engine for watching a directory of YAML sidecars.
// Start(ctx) performs an initial reconcile then watches the directory for changes.
// Current() returns the live Registry at any time.
// ConfigManager implements manager.Manager so it can be wrapped by ConfigSyncService.
type ConfigManager[T executor.Syncable] struct {
	logger  *logger.Logger
	name    string // plugin type label (e.g. "rule"); used in log/error messages
	dir     string
	loader  Loader[T]
	current atomic.Pointer[Registry[T]]

	mu       sync.Mutex           // serialises concurrent reconcile calls
	cache    map[string]T         // path → last successfully loaded item
	modTimes map[string]time.Time // path → mtime at last successful load
}

// NewConfigManager creates a ConfigManager for the given plugin type.
// name is the short plugin type label (e.g. "rule", "enrichment").
func NewConfigManager[T executor.Syncable](logger *logger.Logger, name, dir string, loader Loader[T]) *ConfigManager[T] {
	m := &ConfigManager[T]{
		logger:   logger,
		name:     name,
		dir:      dir,
		loader:   loader,
		cache:    make(map[string]T),
		modTimes: make(map[string]time.Time),
	}
	m.current.Store(buildRegistry([]T(nil))) // ensure Current() never returns nil
	return m
}

// Start performs an initial reconcile, sets up the fsnotify watcher, and spawns
// the watch goroutine. Returns quickly; ongoing work runs in the background.
func (m *ConfigManager[T]) Start(ctx context.Context) error {
	if err := m.reconcile("startup"); err != nil {
		return err
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := fsw.Add(m.dir); err != nil {
		fsw.Close()
		return err
	}

	go func() {
		defer fsw.Close()
		var timer *time.Timer
		// Periodic fallback: on macOS/kqueue, REMOVE events may not fire while a process
		// holds the file descriptor open.
		poll := time.NewTicker(5 * time.Second)
		defer poll.Stop()

		trigger := func(reason string) {
			if err := m.reconcile(reason); err != nil {
				m.logger.ErrorF("reconcile error: %v", err)
			}
		}

		for {
			select {
			case _, ok := <-fsw.Events:
				if !ok {
					return
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(debounce, func() { trigger("debounce") })
			case <-poll.C:
				trigger("poll")
			case err := <-fsw.Errors:
				m.logger.ErrorF("fsnotify error: %v", err)
				trigger("overflow")
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

// Current returns the most recently loaded Registry.
func (m *ConfigManager[T]) Current() *Registry[T] { return m.current.Load() }

// reconcile scans the directory, re-parses files whose mtime changed, removes
// deleted entries, validates the full set, and atomically swaps the Registry.
func (m *ConfigManager[T]) reconcile(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Info("reconciling %s configs (%s)...", m.name, reason)

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", m.dir, err)
	}

	candidate := make(map[string]T, len(m.cache))
	for k, v := range m.cache {
		candidate[k] = v
	}
	candidateMod := make(map[string]time.Time, len(m.modTimes))
	for k, v := range m.modTimes {
		candidateMod[k] = v
	}

	var binaries []string
	seen := make(map[string]struct{}, len(entries))
	changed := false

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0111 != 0 {
			binaries = append(binaries, e.Name())
			continue
		}
		if !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(m.dir, e.Name())
		seen[path] = struct{}{}
		if mt, ok := candidateMod[path]; ok && !info.ModTime().After(mt) {
			continue // mtime unchanged — skip re-parse
		}
		item, err := m.loader.Parse(path)
		if err != nil {
			m.logger.ErrorF("reconcile load %s: %v", path, err)
			continue
		}
		candidate[path] = item
		candidateMod[path] = info.ModTime()
		changed = true
	}
	for path := range candidate {
		if _, present := seen[path]; !present {
			delete(candidate, path)
			delete(candidateMod, path)
			changed = true
		}
	}

	if !changed {
		return nil
	}

	items := make([]T, 0, len(candidate))
	for _, v := range candidate {
		items = append(items, v)
	}

	for _, ve := range m.loader.Validate(items, binaries) {
		m.logger.ErrorF("reconcile validate: %v", ve)
	}

	if err := m.loader.CrossValidate(items); err != nil {
		m.logger.ErrorF("reconcile cross-validate: %v", err)
		return nil // candidate discarded; cache and current unchanged
	}

	m.cache = candidate
	m.modTimes = candidateMod
	m.current.Store(buildRegistry(items))
	m.logger.Info("reconciled %d %s configs", len(items), m.name)
	return nil
}

// liveItemsAndBinaries scans m.dir fresh, parsing all YAML files via loader.Parse,
// and returns the parsed items and executable binary names.
func (m *ConfigManager[T]) liveItemsAndBinaries() ([]T, []string) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, nil
	}
	var items []T
	var binaries []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0111 != 0 {
			binaries = append(binaries, e.Name())
			continue
		}
		if !isYAML(e.Name()) {
			continue
		}
		item, err := m.loader.Parse(filepath.Join(m.dir, e.Name()))
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	return items, binaries
}

// DesiredBinaryState returns the BinaryState for the binary with the given
// filename stem (no extension). Returns false when no YAML sidecar is registered.
func (m *ConfigManager[T]) DesiredBinaryState(name string) (executor.BinaryState, bool) {
	item, ok := m.Current().ByFileName(name)
	if !ok {
		return executor.BinaryState{}, false
	}
	md := item.Metadata()
	return executor.BinaryState{
		ID:       md.Id,
		Name:     md.Name,
		Enabled:  md.Enabled,
		Mode:     md.RolloutMode,
		MaxProcs: md.MaxProcs,
	}, true
}

// HasBlockingErrorFor reports whether there is a blocking validation error matching
// the given plugin ID or YAML file name.
func (m *ConfigManager[T]) HasBlockingErrorFor(id string, yamlFile string) bool {
	items, binaries := m.liveItemsAndBinaries()
	for _, e := range m.loader.Validate(items, binaries) {
		if !e.Blocking {
			continue
		}
		if id != "" && e.PluginID == id {
			return true
		}
		if yamlFile != "" && e.File == yamlFile {
			return true
		}
	}
	return false
}

// HasBlockingError reports whether the given plugin ID has any blocking validation error.
func (m *ConfigManager[T]) HasBlockingError(pluginID string) bool {
	if pluginID == "" {
		return false
	}
	return m.HasBlockingErrorFor(pluginID, "")
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

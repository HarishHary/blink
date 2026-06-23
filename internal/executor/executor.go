package executor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pools"
)

// PluginExecutor[T] is the generic plugin subprocess manager.
// It watches a directory for executable binaries, manages their subprocess lifecycle, and calls notify for Register/Update/Unregister events so the caller can update pools.
type PluginExecutor[T Syncable] struct {
	logger         *logger.Logger
	notify         Notify
	dir            string
	adapter        *PluginAdapter[T]
	metrics        *PluginExecutorMetrics
	mu             sync.RWMutex
	reconcileMu    sync.Mutex // serialises concurrent reconcile calls
	plugin_handles map[string][]*PluginHandle
	failures       map[string]*startFailure
	restarting     map[string]struct{} // paths mid-restart; reconcile skips these to prevent double-start
	pingInterval   time.Duration       // 0 → defaults to 15s; set before Start() for tests
}

// WithPingInterval overrides the default 15-second health-check interval.
// Must be called before Start(). Primarily useful in tests where a short interval
// is needed to detect subprocess crashes quickly without a 15-second wait.
func (m *PluginExecutor[T]) WithPingInterval(d time.Duration) *PluginExecutor[T] {
	m.pingInterval = d
	return m
}

func NewPluginExecutor[T Syncable](logger *logger.Logger, notify Notify, dir string, adapter *PluginAdapter[T], metrics *PluginExecutorMetrics) *PluginExecutor[T] {
	return &PluginExecutor[T]{
		logger:         logger,
		notify:         notify,
		dir:            dir,
		adapter:        adapter,
		metrics:        metrics,
		plugin_handles: make(map[string][]*PluginHandle),
		failures:       make(map[string]*startFailure),
		restarting:     make(map[string]struct{}),
	}
}

// Performs an initial reconcile then watches the plugin directory for changes.
func (m *PluginExecutor[T]) Start(ctx context.Context) error {
	if err := m.reconcile("initial"); err != nil {
		return err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(m.dir); err != nil {
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		var timer *time.Timer
		debounce := 400 * time.Millisecond
		// Periodic fallback: on macOS/kqueue, REMOVE events may not fire while a running
		// subprocess holds the binary's fd open. A 5-second poll catches those gaps,
		// and also picks up YAML sidecar changes that disable/remove rules.
		poll := time.NewTicker(5 * time.Second)
		defer poll.Stop()

		trigger := func(reason string) {
			if err := m.reconcile(reason); err != nil {
				m.logger.ErrorF("reconcile error: %v", err)
			}
		}

		for {
			select {
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				// Reconcile on any file event - reconcile() skips non-executables when iterating.
				// Reacting to YAML events too ensures a binary deferred by IsReady (waiting for
				// its sidecar) is picked up within the debounce window, not the next 5s poll.
				// AfterFunc timers have no drainable C channel - just Stop and replace.
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(debounce, func() { trigger("debounce") })
			case <-poll.C:
				trigger("poll")
			case err := <-w.Errors:
				m.logger.ErrorF("fsnotify error: %v", err)
				trigger("overflow")
			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

func (m *PluginExecutor[T]) reconcile(reason string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	m.logger.Info("reconciling %s plugins (%s)...", m.adapter.PluginKey(), reason)

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}

	// newBinary collects binaries that need a fresh start this reconcile cycle.
	// They are sorted stable-first before starting so that, on a fresh pod start with
	// both binaries on disk, the stable (non-shadow) version always wins the active slot
	// in the pool regardless of filename alphabetical order.
	type newBinary struct {
		path   string
		hash   string
		shadow bool
	}
	var toStart []newBinary

	var modeOnlyChanges []modeOnlyChange

	seen := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(m.dir, e.Name())
		info, err := e.Info()
		if err != nil || info.Mode()&0111 == 0 {
			continue // skip non-executables
		}
		h, err := helpers.BinaryChecksum(path)
		if err != nil {
			m.logger.ErrorF("hash %s: %v", path, err)
			continue
		}
		seen[path] = struct{}{}

		m.mu.RLock()
		handles, exists := m.plugin_handles[path]
		_, pending := m.restarting[path]
		m.mu.RUnlock()

		if pending {
			continue // pingLoop is already handling the restart
		}

		if exists {
			hashChanged := handles[0].Key.Hash != h
			modeChanged := handles[0].Mode != m.adapter.CurrentMode(path)
			if !hashChanged && !modeChanged {
				continue // binary and mode unchanged
			}
			if !m.adapter.IsReady(path) {
				if modeChanged {
					// Operator changed the mode to an invalid combination (e.g. CN→BG
					// while another BG binary already holds active). Kill the process
					// now — no tombstone, so it restarts automatically once the YAML
					// is fixed (e.g. the stable binary is demoted simultaneously).
					m.logger.Info("%s %s: mode change creates invalid config, stopping until YAML is fixed", m.adapter.PluginKey(), path)
					m.stop(path, handles)
				} else {
					m.logger.Info("%s %s: change detected but prerequisites not ready, deferring update", m.adapter.PluginKey(), path)
				}
				continue
			}
			if !hashChanged && modeChanged {
				// Pure YAML mode change — no new binary, no kill needed.
				// Collect for atomic slot migration after the full scan so that
				// two-binary swaps (e.g. BG↔CN/SH) are applied together.
				modeOnlyChanges = append(modeOnlyChanges, modeOnlyChange{
					path:    path,
					handles: handles,
					newMode: m.adapter.CurrentMode(path),
				})
				continue
			}
			if err := m.update(path, handles, h); err != nil {
				m.logger.ErrorF("update %s %s: %v", m.adapter.PluginKey(), path, err)
			}
			continue
		}

		if !m.adapter.IsReady(path) {
			m.logger.Info("%s %s: prerequisites not ready, deferring start", m.adapter.PluginKey(), path)
			continue
		}
		toStart = append(toStart, newBinary{path: path, hash: h, shadow: m.adapter.IsShadow(path)})
	}

	// Stable (non-shadow) binaries first: false < true keeps them at the front.
	sort.Slice(toStart, func(i, j int) bool {
		return !toStart[i].shadow && toStart[j].shadow
	})
	for _, nb := range toStart {
		if err := m.startWithBackoff(nb.path, nb.hash); err != nil {
			m.logger.ErrorF("start %s %s: %v", m.adapter.PluginKey(), nb.path, err)
		}
	}

	m.applyModeChanges(modeOnlyChanges)

	// Collect plugins that need to be stopped or removed, then act outside the lock
	// so that kill() (gRPC Shutdown, up to 3s) does not block readers.
	type pendingAction struct {
		key     string
		handles []*PluginHandle
		perm    bool // true = binary deleted (remove); false = disabled (stop)
	}
	var pending []pendingAction
	m.mu.RLock()
	for key, handles := range m.plugin_handles {
		_, present := seen[key]
		if !present {
			pending = append(pending, pendingAction{key, handles, true})
		} else if !m.adapter.IsEnabled(handles[0]) {
			pending = append(pending, pendingAction{key, handles, false})
		}
	}
	m.mu.RUnlock()

	for _, p := range pending {
		if p.perm {
			m.remove(p.key, p.handles)
		} else {
			m.stop(p.key, p.handles)
		}
	}
	return nil
}

type modeOnlyChange struct {
	path    string
	handles []*PluginHandle
	newMode pools.RolloutMode
}

// applyModeChanges handles pure YAML mode changes (hash unchanged) without killing
// or spawning any processes. Changes are grouped by plugin ID so that two-binary
// swaps (one going BG, the other going CN/SH) are applied atomically via a single
// MigrateSlots call. Single-binary mode changes only update the mode snapshot so
// the next reconcile does not re-detect the same change.
func (m *PluginExecutor[T]) applyModeChanges(changes []modeOnlyChange) {
	if len(changes) == 0 {
		return
	}

	// Group by plugin ID.
	byID := make(map[string][]modeOnlyChange, len(changes))
	for _, c := range changes {
		id := c.handles[0].Key.Id
		byID[id] = append(byID[id], c)
	}

	for _, group := range byID {
		if len(group) == 2 {
			// Detect a swap: one binary becoming BG, the other CN/SH.
			var bgC, altC *modeOnlyChange
			for i := range group {
				if group[i].newMode == pools.RolloutModeBlueGreen {
					bgC = &group[i]
				} else {
					altC = &group[i]
				}
			}
			if bgC != nil && altC != nil {
				// Atomically reassign slots — no kill, no spawn.
				m.notify(NewMigrateMessage[T](bgC.handles[0].Key, altC.handles[0].Key))
				m.logger.Info("%s: slot swap — %s (BG) → active, %s (%s) → pending",
					m.adapter.PluginKey(), bgC.path, altC.path, altC.newMode)
				m.mu.Lock()
				for _, h := range bgC.handles {
					h.Mode = bgC.newMode
				}
				for _, h := range altC.handles {
					h.Mode = altC.newMode
				}
				m.mu.Unlock()
				continue
			}
		}
		// Single binary or both moving to same category: slot stays unchanged.
		// Just update the mode snapshot so the next reconcile does not re-trigger.
		for _, c := range group {
			m.logger.Info("%s %s: mode updated to %s (slot unchanged)", m.adapter.PluginKey(), c.path, c.newMode)
			m.mu.Lock()
			for _, h := range c.handles {
				h.Mode = c.newMode
			}
			m.mu.Unlock()
		}
	}
}

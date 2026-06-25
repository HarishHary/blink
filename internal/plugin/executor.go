package plugin

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/controller/model"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pools"
)

// SnapshotSource is the executor's view of the control plane: the latest desired
// state plus a notification when it changes. *controller.Replica satisfies it.
type SnapshotSource interface {
	Snapshot() *model.Snapshot
	Subscribe() (<-chan struct{}, func())
}

// PluginExecutor[T] is the generic plugin subprocess manager.
// It reconciles running subprocesses against the control plane's desired state
// (a model.Snapshot delivered via SnapshotSource), manages their lifecycle, and
// calls notify for Register/Update/Unregister events so the caller can update pools.
// The directory remains the local artifact store: snapshot refs name binaries that
// resolve() maps to files on disk.
type PluginExecutor[T Syncable] struct {
	logger         *logger.Logger
	notify         Notify
	dir            string
	snapshot       SnapshotSource
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

func NewPluginExecutor[T Syncable](logger *logger.Logger, notify Notify, dir string, snapshot SnapshotSource, adapter *PluginAdapter[T], metrics *PluginExecutorMetrics) *PluginExecutor[T] {
	return &PluginExecutor[T]{
		logger:         logger,
		notify:         notify,
		dir:            dir,
		snapshot:       snapshot,
		adapter:        adapter,
		metrics:        metrics,
		plugin_handles: make(map[string][]*PluginHandle),
		failures:       make(map[string]*startFailure),
		restarting:     make(map[string]struct{}),
	}
}

// resolve maps an artifact Name from the snapshot to a runnable binary path.
// ponytail: the directory is the artifact store today; swap this body for an
// artifact-store fetch when the local plugin directory is removed.
func (m *PluginExecutor[T]) resolve(name string) string {
	return filepath.Join(m.dir, name)
}

// Start subscribes to control-plane snapshot changes, performs an initial reconcile,
// then re-reconciles on every snapshot update until ctx is cancelled.
// Subscribe happens before the initial reconcile so a snapshot arriving concurrently
// (the replica is a sibling service) is never missed: the cap-1 watcher channel retains
// the signal and the loop re-reconciles. reconcile is idempotent, so a double run is safe.
func (m *PluginExecutor[T]) Start(ctx context.Context) error {
	ch, unsubscribe := m.snapshot.Subscribe()
	if err := m.reconcile("initial"); err != nil {
		unsubscribe()
		return err
	}

	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ch:
				if err := m.reconcile("snapshot"); err != nil {
					m.logger.ErrorF("%s reconcile error: %v", m.adapter.PluginKey(), err)
				}
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

	snap := m.snapshot.Snapshot()
	if snap == nil {
		// Control plane has not delivered a snapshot yet. Nothing desired, nothing to do.
		m.logger.Info("%s reconcile (%s): no snapshot yet", m.adapter.PluginKey(), reason)
		return nil
	}
	m.logger.Info("reconciling %s plugins (%s, generation=%d)...", m.adapter.PluginKey(), reason, snap.Generation)

	// Desired state comes from the snapshot: each enabled entry contributes its primary
	// and candidate refs. resolve() maps each ref Name to a binary in the artifact store.
	// Mode is authoritative from the snapshot ref (the controller derived it). Worker count
	// and readiness still come from the YAML sidecar via the adapter until those move into
	// the snapshot.
	//
	// ponytail: readiness (IsReady) reads the local YAML while reconcile only triggers on
	// snapshot changes — a binary deferred as not-ready retries on the next snapshot, not on
	// a YAML edit. Harmless today (the controller derives the snapshot from that same YAML,
	// and a Kafka snapshot delivery is slower than the local config load); removed entirely
	// when workers/readiness move into the snapshot and the YAML sidecar goes away.
	type desired struct {
		path   string
		hash   string
		mode   pools.RolloutMode
		shadow bool
	}
	wanted := make(map[string]desired)     // path → desired binary (enabled)
	disabled := make(map[string]struct{})  // paths the controller knows but has disabled → stop, not remove
	for _, e := range snap.Entries {
		for _, ref := range [...]*model.ArtifactRef{e.Primary, e.Candidate} {
			if ref == nil {
				continue
			}
			path := m.resolve(ref.Name)
			if !e.Enabled {
				disabled[path] = struct{}{}
				continue
			}
			h, err := helpers.BinaryChecksum(path)
			if err != nil {
				m.logger.ErrorF("%s hash %s: %v", m.adapter.PluginKey(), path, err)
				continue // artifact not resolvable locally; skip until it is
			}
			shadow := ref.Mode == pools.RolloutModeCanary || ref.Mode == pools.RolloutModeShadow
			wanted[path] = desired{path: path, hash: h, mode: ref.Mode, shadow: shadow}
		}
	}

	// toStart collects binaries needing a fresh start this cycle. They are sorted
	// stable-first so that, on a fresh pod start with both versions present, the stable
	// (non-shadow) version always wins the active pool slot regardless of map order.
	var toStart []desired
	var modeOnlyChanges []modeOnlyChange

	for path, w := range wanted {
		m.mu.RLock()
		handles, exists := m.plugin_handles[path]
		_, pending := m.restarting[path]
		m.mu.RUnlock()

		if pending {
			continue // pingLoop is already handling the restart
		}

		if exists {
			hashChanged := handles[0].Key.Hash != w.hash
			modeChanged := handles[0].Mode != w.mode
			if !hashChanged && !modeChanged {
				continue // binary and mode unchanged
			}
			if !m.adapter.IsReady(path) {
				if modeChanged {
					// Mode change creates an invalid combination (e.g. CN→BG while another
					// BG binary already holds active). Kill now — no tombstone, so it
					// restarts automatically once the config is consistent again.
					m.logger.Info("%s %s: mode change creates invalid config, stopping until ready", m.adapter.PluginKey(), path)
					m.stop(path, handles)
				} else {
					m.logger.Info("%s %s: change detected but prerequisites not ready, deferring update", m.adapter.PluginKey(), path)
				}
				continue
			}
			if !hashChanged && modeChanged {
				// Pure mode change — no new binary, no kill needed.
				// Collect for atomic slot migration after the full pass so that
				// two-binary swaps (e.g. BG↔CN/SH) are applied together.
				modeOnlyChanges = append(modeOnlyChanges, modeOnlyChange{
					path:    path,
					handles: handles,
					newMode: w.mode,
				})
				continue
			}
			if err := m.update(path, handles, w.hash); err != nil {
				m.logger.ErrorF("update %s %s: %v", m.adapter.PluginKey(), path, err)
			}
			continue
		}

		if !m.adapter.IsReady(path) {
			m.logger.Info("%s %s: prerequisites not ready, deferring start", m.adapter.PluginKey(), path)
			continue
		}
		toStart = append(toStart, w)
	}

	// Stable (non-shadow) binaries first: false < true keeps them at the front.
	sort.Slice(toStart, func(i, j int) bool {
		return !toStart[i].shadow && toStart[j].shadow
	})
	for _, w := range toStart {
		if err := m.startWithBackoff(w.path, w.hash); err != nil {
			m.logger.ErrorF("start %s %s: %v", m.adapter.PluginKey(), w.path, err)
		}
	}

	m.applyModeChanges(modeOnlyChanges)

	// Stop or remove running handles the snapshot no longer wants. Act outside the lock
	// so kill() (gRPC Shutdown, up to 3s) does not block readers.
	//   - disabled in the snapshot → stop  (Unregister, no tombstone — may be re-enabled)
	//   - absent from the snapshot → remove (Remove, tombstone — the ID is gone)
	type pendingAction struct {
		key     string
		handles []*PluginHandle
		perm    bool
	}
	var pending []pendingAction
	m.mu.RLock()
	for key, handles := range m.plugin_handles {
		if _, keep := wanted[key]; keep {
			continue
		}
		_, isDisabled := disabled[key]
		pending = append(pending, pendingAction{key, handles, !isDisabled})
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

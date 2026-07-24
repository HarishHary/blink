package plugin

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/snapshot"
)

// SnapshotSource is the executor's view of the control plane: latest desired state plus a change signal (satisfied by *controller.SnapshotReader).
type SnapshotSource interface {
	Snapshot() *snapshot.Snapshot
	Subscribe() (<-chan struct{}, func())
}

// PluginExecutor[T] reconciles running subprocesses against the control plane's snapshot (via SnapshotSource),
// manages their lifecycle, and notifies the pool of Register/Update/Unregister events. dir is the artifact store.
type PluginExecutor[T Syncable] struct {
	logger         *logger.Logger
	notify         Notify
	dir            string
	src            SnapshotSource
	adapter        *PluginAdapter[T]
	metrics        *PluginExecutorMetrics
	mu             sync.RWMutex
	reconcileMu    sync.Mutex // serialises concurrent reconcile calls
	plugin_handles map[string][]*PluginHandle
	failures       map[string]*startFailure
	restarting     map[string]struct{} // paths mid-restart; reconcile skips these to prevent double-start
	pingInterval   time.Duration       // 0 → defaults to 15s; set before Start() for tests
	retryTimer     *time.Timer         // coalesced wake-up for backoff retries; nil when none pending (guarded by mu)
	retryAt        time.Time           // deadline of the pending retryTimer (guarded by mu)
}

// WithPingInterval overrides the default 15s health-check interval; call before Start() (mainly for tests).
func (m *PluginExecutor[T]) WithPingInterval(d time.Duration) *PluginExecutor[T] {
	m.pingInterval = d
	return m
}

func NewPluginExecutor[T Syncable](logger *logger.Logger, notify Notify, dir string, snapshotSrc SnapshotSource, adapter *PluginAdapter[T], metrics *PluginExecutorMetrics) *PluginExecutor[T] {
	return &PluginExecutor[T]{
		logger:         logger,
		notify:         notify,
		dir:            dir,
		src:            snapshotSrc,
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

// Start subscribes, does an initial reconcile, then re-reconciles on each snapshot change until ctx is cancelled.
// Subscribe-before-reconcile means a concurrently-arriving snapshot is never missed (cap-1 signal retained; reconcile is idempotent).
func (m *PluginExecutor[T]) Start(ctx context.Context) error {
	ch, unsubscribe := m.src.Subscribe()
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
				// ponytail: a retry callback already past its nil-check may still run one
				// reconcile after cancellation, consistent with the executor's loose shutdown.
				m.mu.Lock()
				if m.retryTimer != nil {
					m.retryTimer.Stop()
					m.retryTimer = nil
				}
				m.mu.Unlock()
				return
			}
		}
	}()

	return nil
}

func (m *PluginExecutor[T]) reconcile(reason string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	snap := m.src.Snapshot()
	if snap == nil {
		// Control plane has not delivered a snapshot yet. Nothing desired, nothing to do.
		m.logger.Info("%s reconcile (%s): no snapshot yet", m.adapter.PluginKey(), reason)
		return nil
	}
	m.logger.Info("reconciling %s plugins (%s, generation=%d)...", m.adapter.PluginKey(), reason, snap.Generation)

	// Desired state from the snapshot: each enabled entry's primary+candidate refs. resolve() maps ref.Name to
	// a binary; mode is authoritative from the ref; worker count/readiness come from the snapshot-backed DesiredConfig.
	type desired struct {
		path   string
		hash   string
		mode   pools.RolloutMode
		shadow bool
	}
	wanted := make(map[string]desired)    // path → desired binary (enabled)
	disabled := make(map[string]struct{}) // paths the controller knows but has disabled → stop, not remove
	deferred := make(map[string]struct{}) // paths whose local binary is unresolvable or fails digest verification → keep the current version running, do not upgrade or tear down
	for _, e := range snap.Entries {
		for _, ref := range [...]*snapshot.ArtifactRef{e.Primary, e.Candidate} {
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
				// Binary missing/unreadable: defer like a digest mismatch (keep any running version, don't start).
				// Else this enabled path, in no bucket, would be torn down + tombstoned over a transient artifact blip.
				m.logger.ErrorF("%s hash %s: %v (deferring - keeping current version, not starting)", m.adapter.PluginKey(), path, err)
				deferred[path] = struct{}{}
				continue
			}
			// Digest mismatch = wrong/tampered/not-yet-deployed binary: defer (keep running, don't upgrade; never tears down a healthy process).
			// Empty ref.Hash = pre-digest snapshot; trust the local binary.
			if ref.Hash != "" && h != ref.Hash {
				m.metrics.DigestMismatches.Inc()
				m.logger.ErrorF("%s %s: local binary digest %s != published %s; deferring (keeping current version, not upgrading)", m.adapter.PluginKey(), path, h, ref.Hash)
				deferred[path] = struct{}{}
				continue
			}
			shadow := ref.RolloutMode == pools.RolloutModeCanary || ref.RolloutMode == pools.RolloutModeShadow
			wanted[path] = desired{path: path, hash: h, mode: ref.RolloutMode, shadow: shadow}
		}
	}

	// toStart collects fresh starts, sorted stable-first so on a cold pod the stable (non-shadow) version wins the active slot regardless of map order.
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
					// Mode change creates an invalid combo (e.g. CN→BG while another BG holds active): kill now, no tombstone - restarts once config is consistent.
					m.logger.Info("%s %s: mode change creates invalid config, stopping until ready", m.adapter.PluginKey(), path)
					m.stop(path, handles)
				} else {
					m.logger.Info("%s %s: change detected but prerequisites not ready, deferring update", m.adapter.PluginKey(), path)
				}
				continue
			}
			if !hashChanged && modeChanged {
				// Pure mode change (no new binary, no kill): collect for atomic slot migration after the full pass so two-binary swaps apply together.
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

	// Stop/remove handles the snapshot no longer wants, outside the lock (kill() blocks up to 3s on gRPC Shutdown):
	// disabled → stop (no tombstone, may re-enable); absent → remove (tombstone, ID is gone).
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
		if _, def := deferred[key]; def {
			continue // digest mismatch: keep the running version until a matching binary appears
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

// applyModeChanges applies pure mode flips (hash unchanged) with no kill/spawn: a two-binary BG↔CN/SH swap goes
// through one atomic MigrateSlots; a single-binary flip just updates the mode label so the next reconcile doesn't re-detect it.
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
				// Atomically reassign slots - no kill, no spawn.
				m.notify(NewMigrateMessage[T](bgC.handles[0].Key, altC.handles[0].Key))
				m.logger.Info("%s: slot swap - %s (BG) → active, %s (%s) → pending",
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
		// Single binary (or both to same category): slot unchanged, just update the mode label so the next reconcile doesn't re-trigger.
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

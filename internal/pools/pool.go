package pools

import (
	"errors"
	"log"
	"sync"
	"time"
)

// Returned by Call when no active pool exists for the requested
var ErrPluginNotFound = errors.New("plugin not found")

// Returned by Call when the plugin was explicitly deregistered (binary was deleted).
var ErrPluginRemoved = errors.New("plugin removed")

// holds a pre-warmed pool that is waiting to be promoted to active via Promote().
// Used for canary and shadow rollouts where traffic must stay on the old version until the operator explicitly graduates the new version.
type pendingPromotion struct {
	key       PoolKey
	onDrained func()
}

// Manages VersionedPools keyed by (Id, Version).
type ProcessPool[T any] struct {
	mu           sync.RWMutex
	pools        map[PoolKey]*VersionedPool[T]
	active       map[string]PoolKey
	pending      map[string]pendingPromotion
	removed      map[string]struct{}
	routing      RoutingConfig
	drainTimeout time.Duration // drainTimeout ≤ 0 uses 60s.
	metrics      *PoolMetrics
}

const defaultDrainTimeout = 60 * time.Second

// Creates a ProcessPool driven by the given RoutingConfig callback.
func NewProcessPool[T any](routing RoutingConfig, metrics *PoolMetrics, drainTimeout time.Duration) *ProcessPool[T] {
	if drainTimeout <= 0 {
		drainTimeout = defaultDrainTimeout
	}
	return &ProcessPool[T]{
		pools:        make(map[PoolKey]*VersionedPool[T]),
		active:       make(map[string]PoolKey),
		pending:      make(map[string]pendingPromotion),
		removed:      make(map[string]struct{}),
		routing:      routing,
		drainTimeout: drainTimeout,
		metrics:      metrics,
	}
}

// Register adds a pre-warmed pool for the given key.
//
// Blue-green (default): the new pool is promoted to active immediately and the old pool
// is drained asynchronously. onDrained is called once the drain completes
//
// Canary / Shadow: the new pool is added to pp.pools but active is NOT flipped. The old
// pool keeps serving production traffic; the new pool serves only the canary/shadow
// percentage as found by callCanary/callShadow. Call Promote(pluginID) to graduate the
// new pool to production and drain the old one.
func (pp *ProcessPool[T]) Register(key PoolKey, plugins []T, maxProcs int, onDrained func()) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	pool := newVersionedPool(key, plugins, maxProcs)
	pp.pools[key] = pool
	if pp.metrics != nil {
		pp.metrics.poolSize.WithLabelValues(key.Id, key.Name).Set(float64(pool.Size()))
	}

	// Clear tombstone: plugin has come back (re-deployed after deletion).
	delete(pp.removed, key.Id)

	mode, _ := pp.routing(key.Id, key.Name)

	if mode == RolloutModeCanary || mode == RolloutModeShadow {
		// Stage the new pool without promoting - preserve active as production.
		// First registration for this pluginID still needs an active entry.
		if _, hasActive := pp.active[key.Id]; !hasActive {
			pp.active[key.Id] = key
		} else {
			// Drain the previous pending pool before replacing it so its subprocess
			// is killed and its onDrained callback fires. Without this, rapid deploys
			// in canary mode would orphan intermediate pools in pp.pools indefinitely.
			if prev, ok := pp.pending[key.Id]; ok {
				if prevPool, ok := pp.pools[prev.key]; ok {
					go pp.drain(prev.key, prevPool, prev.onDrained)
				}
			}
			pp.pending[key.Id] = pendingPromotion{key: key, onDrained: onDrained}
		}
		return
	}

	// Blue-green: promote immediately and drain old.
	// Two co-existing blue-green binaries for the same plugin ID are prevented from ever
	// reaching this point: IsReady() calls HasBlockingError() which runs Validate() fresh,
	// and Validate() emits a blocking error for any plugin ID with multiple blue-green versions.
	oldKey, hasOld := pp.active[key.Id]
	pp.active[key.Id] = key

	if hasOld && oldKey != key {
		if oldPool, ok := pp.pools[oldKey]; ok {
			go pp.drain(oldKey, oldPool, onDrained)
		}
	}
}

// MigrateSlots atomically reassigns active and pending for pluginID without draining
// or killing any processes. Used when two binaries for the same id swap modes
// (e.g. BG↔CN/SH) via a YAML-only change — no binary restarts are needed.
// Pass a zero PoolKey for pendingKey to clear the pending slot.
func (pp *ProcessPool[T]) MigrateSlots(id string, activeKey, pendingKey PoolKey) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.active[id] = activeKey
	if pendingKey != (PoolKey{}) {
		existing, ok := pp.pending[id]
		if !ok || existing.key != pendingKey {
			pp.pending[id] = pendingPromotion{key: pendingKey}
		}
	} else {
		delete(pp.pending, id)
	}
}

// Promote graduates the pending canary/shadow pool for pluginID to active production,
// draining the old pool asynchronously. If no pending pool exists, this is a no-op.
// Typically called by an operator API or a health-check once canary metrics are green.
func (pp *ProcessPool[T]) Promote(pluginID string) {
	pp.mu.Lock()

	p, ok := pp.pending[pluginID]
	if !ok {
		pp.mu.Unlock()
		return
	}
	delete(pp.pending, pluginID)

	oldKey, hasOld := pp.active[pluginID]
	pp.active[pluginID] = p.key

	var drainPool *VersionedPool[T]
	var drainKey PoolKey
	noOldPool := false
	if hasOld && oldKey != p.key {
		drainPool = pp.pools[oldKey]
		drainKey = oldKey
		noOldPool = drainPool == nil
	}
	pp.mu.Unlock()

	// Call onDrained outside the lock - it may run kill() which blocks for up to 3s
	// on gRPC Shutdown. Holding the lock that long would stall all Call() invocations.
	switch {
	case drainPool != nil:
		go pp.drain(drainKey, drainPool, p.onDrained)
	case !hasOld || oldKey == p.key:
		// No old pool to drain (first registration or same key promoted) - fire callback directly.
		if p.onDrained != nil {
			p.onDrained()
		}
	case noOldPool:
		// Old key existed in active but pool was already removed - skip onDrained.
	}
}

// Unregister drains the specific versioned pool identified by key.
// Used for transient stops (crash restarts, config disables) — no tombstone is set.
// Only the pool that crashed is torn down; other versions of the same plugin are unaffected.
func (pp *ProcessPool[T]) Unregister(key PoolKey) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	// If it's the pending (canary/shadow) pool, drain it only.
	if p, ok := pp.pending[key.Id]; ok && p.key == key {
		delete(pp.pending, key.Id)
		if pool, ok := pp.pools[p.key]; ok {
			go pp.drain(p.key, pool, p.onDrained)
		}
		return
	}

	// If it's the active pool, drain it only.
	activeKey, ok := pp.active[key.Id]
	if !ok || activeKey != key {
		return
	}
	delete(pp.active, key.Id)
	if pool, ok := pp.pools[activeKey]; ok {
		go pp.drain(activeKey, pool, nil)
	}
}

// Remove drains the specific versioned pool identified by key and tombstones the plugin ID
// only when no other pools for that plugin remain.
// Used when a binary is permanently deleted from disk.
func (pp *ProcessPool[T]) Remove(key PoolKey) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	if p, ok := pp.pending[key.Id]; ok && p.key == key {
		delete(pp.pending, key.Id)
		if pool, ok := pp.pools[p.key]; ok {
			go pp.drain(p.key, pool, p.onDrained)
		}
	} else if activeKey, ok := pp.active[key.Id]; ok && activeKey == key {
		delete(pp.active, key.Id)
		if pool, ok := pp.pools[activeKey]; ok {
			go pp.drain(activeKey, pool, nil)
		}
	} else {
		// Key not currently tracked — tombstone so callers don't wait forever.
		pp.removed[key.Id] = struct{}{}
		return
	}

	// Tombstone only if no pools remain for this plugin.
	_, hasActive := pp.active[key.Id]
	_, hasPending := pp.pending[key.Id]
	if !hasActive && !hasPending {
		pp.removed[key.Id] = struct{}{}
	}
}

// marks the VersionedPool as draining, waits for in-flight calls to finish
// (up to pp.drainTimeout), removes it from pp.pools, then calls onDrained if set.
// For graceful updates, onDrained kills the old subprocess after the last in-flight
// call completes so no call ever hits a dead gRPC connection.
func (pp *ProcessPool[T]) drain(key PoolKey, pool *VersionedPool[T], onDrained func()) {
	pool.draining.Store(true)
	deadline := time.Now().Add(pp.drainTimeout)
	start := time.Now()

	for time.Now().Before(deadline) {
		if pool.Inflight() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	elapsed := time.Since(start).Seconds()
	if pp.metrics != nil {
		pp.metrics.drainDuration.WithLabelValues(key.Id, key.Name).Observe(elapsed)
		pp.metrics.poolSize.WithLabelValues(key.Id, key.Name).Set(0)
		pp.metrics.poolInflight.WithLabelValues(key.Id, key.Name).Set(0)
	}

	if pool.Inflight() > 0 {
		log.Printf("processpool: force-killed pool %s after %.1fs drain (%d in-flight)", key, elapsed, pool.Inflight())
	} else {
		log.Printf("processpool: drained pool %s in %.2fs", key, elapsed)
	}

	// Only delete if this exact pool is still registered at this key.
	// A concurrent Register() may have replaced it while we were waiting.
	pp.mu.Lock()
	if pp.pools[key] == pool {
		delete(pp.pools, key)
	}
	pp.mu.Unlock()

	if onDrained != nil {
		onDrained()
	}
}

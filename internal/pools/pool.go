package pools

import (
	"errors"
	"log"
	"sync"
	"time"
)

// ErrPluginNotFound is returned by Call when no active pool exists for the plugin ID.
var ErrPluginNotFound = errors.New("plugin not found")

// Returned by Call when the plugin was explicitly deregistered (binary was deleted).
var ErrPluginRemoved = errors.New("plugin removed")

// pendingPoolKey is a staged canary/shadow pool awaiting Promote(); production stays on the old version until then.
type pendingPoolKey struct {
	key       PoolKey
	onDrained func()
}

// ProcessPool manages VersionedPools keyed by PoolKey and routes calls by rollout mode.
type ProcessPool[T any] struct {
	mu           sync.RWMutex
	pools        map[PoolKey]*VersionedPool[T]
	active       map[string]PoolKey        // pluginID → active PoolKey
	pending      map[string]pendingPoolKey // pluginID → pending PoolKey (canary/shadow)
	removed      map[string]struct{}       // pluginID tombstone for removed binaries (no pools remain)
	rolloutCfg   RolloutConfig
	drainTimeout time.Duration // drainTimeout ≤ 0 uses 60s.
	metrics      *PoolMetrics
}

const defaultDrainTimeout = 60 * time.Second

// Creates a ProcessPool driven by the given RolloutConfig callback.
func NewProcessPool[T any](rolloutCfg RolloutConfig, metrics *PoolMetrics, drainTimeout time.Duration) *ProcessPool[T] {
	if drainTimeout <= 0 {
		drainTimeout = defaultDrainTimeout
	}
	return &ProcessPool[T]{
		pools:        make(map[PoolKey]*VersionedPool[T]),
		active:       make(map[string]PoolKey),
		pending:      make(map[string]pendingPoolKey),
		removed:      make(map[string]struct{}),
		rolloutCfg:   rolloutCfg,
		drainTimeout: drainTimeout,
		metrics:      metrics,
	}
}

// Register adds a pre-warmed pool for key: blue-green promotes to active immediately (draining the old async); canary/shadow stage to pending until Promote.
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

	mode, _ := pp.rolloutCfg(key.Id, key.Name)

	if mode == RolloutModeCanary || mode == RolloutModeShadow {
		// Stage without promoting; the first registration for this ID still needs an active entry.
		if _, hasActive := pp.active[key.Id]; !hasActive {
			pp.active[key.Id] = key
		} else {
			// Drain the previous pending pool first, else rapid canary deploys orphan intermediate pools.
			if prev, ok := pp.pending[key.Id]; ok {
				if prevPool, ok := pp.pools[prev.key]; ok {
					go pp.drain(prev.key, prevPool, prev.onDrained)
				}
			}
			pp.pending[key.Id] = pendingPoolKey{key: key, onDrained: onDrained}
		}
		return
	}

	// Blue-green: promote immediately, drain old. Two BG binaries for one ID never reach here (Validate() blocks that group upstream).
	oldKey, hasOld := pp.active[key.Id]
	pp.active[key.Id] = key

	if hasOld && oldKey != key {
		if oldPool, ok := pp.pools[oldKey]; ok {
			go pp.drain(oldKey, oldPool, onDrained)
		}
	}
}

// MigrateSlots atomically reassigns active/pending for id with no drain or restart (BG↔CN/SH YAML-only swap). A zero pendingKey clears the pending slot.
func (pp *ProcessPool[T]) MigrateSlots(id string, activeKey, pendingKey PoolKey) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.active[id] = activeKey
	if pendingKey != (PoolKey{}) {
		existing, ok := pp.pending[id]
		if !ok || existing.key != pendingKey {
			pp.pending[id] = pendingPoolKey{key: pendingKey}
		}
	} else {
		delete(pp.pending, id)
	}
}

// Promote graduates the pending canary/shadow pool for pluginID to active, draining the old async. No-op if nothing is pending.
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

	// onDrained runs outside the lock: it may kill() (up to 3s on gRPC Shutdown), which would stall every Call().
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

// Unregister drains just the pool identified by key (active or pending); no tombstone - for transient stops (crash/disable) where the plugin may return.
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

// Remove drains the pool identified by key and tombstones the ID only if no pools for it remain (binary permanently deleted from disk).
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
		// Key not currently tracked - tombstone so callers don't wait forever.
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

// drain marks the pool draining, waits for Inflight()==0 (up to drainTimeout), removes it, then fires onDrained (which kills the old subprocess, so no call hits a dead conn).
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

	// Only delete if this exact pool is still at key - a concurrent Register() may have replaced it.
	pp.mu.Lock()
	if pp.pools[key] == pool {
		delete(pp.pools, key)
	}
	pp.mu.Unlock()

	if onDrained != nil {
		onDrained()
	}
}

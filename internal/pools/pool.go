package pools

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Returned by Call when no active pool exists for the requested
var ErrPluginNotFound = errors.New("plugin not found")

// Returned by Call when the plugin was explicitly deregistered (binary was deleted).
var ErrPluginRemoved = errors.New("plugin removed")

// RolloutMode controls how traffic is split between old and new plugin versions.
type RolloutMode int

const (
	// RolloutModeBlueGreen (default): pre-warm new pool, flip generation, drain old.
	RolloutModeBlueGreen RolloutMode = iota
	// RolloutModeCanary: route RolloutPct% of calls (by consistent hash) to the new version.
	RolloutModeCanary
	// RolloutModeShadow: call new version in background; discard result; log errors.
	RolloutModeShadow
)

func (m RolloutMode) String() string {
	switch m {
	case RolloutModeBlueGreen:
		return "blue-green"
	case RolloutModeCanary:
		return "canary"
	case RolloutModeShadow:
		return "shadow"
	default:
		return fmt.Sprintf("RolloutMode(%d)", int(m))
	}
}

func (m RolloutMode) MarshalText() ([]byte, error) {
	return []byte(m.String()), nil
}

func (m *RolloutMode) UnmarshalText(b []byte) error {
	switch string(b) {
	case "blue-green", "bluegreen", "":
		*m = RolloutModeBlueGreen
	case "canary":
		*m = RolloutModeCanary
	case "shadow":
		*m = RolloutModeShadow
	default:
		return fmt.Errorf("unknown rollout mode %q", string(b))
	}
	return nil
}

// func stub that returns per-plugin routing parameters.
// Return zero values for default blue-green behaviour.
type RoutingConfig func(pluginID string) (mode RolloutMode, rolloutPct float64)

// PoolKey uniquely identifies a versioned plugin subprocess pool.
type PoolKey struct {
	Id      string
	Version string
	Hash    string // SHA-256 of the binary; empty when not yet known
}

func (k PoolKey) String() string {
	if k.Hash != "" {
		return k.Id + "@" + k.Version + "@" + k.Hash
	}
	return k.Id + "@" + k.Version
}

// VersionedPool manages a fixed-size pool of plugin subprocess handles of type T.
// Acquire/Release use a channel-based semaphore; handles are stateful gRPC connections
// and must not be discarded by the GC (no sync.Pool).
type VersionedPool[T any] struct {
	key      PoolKey
	slots    chan T
	inflight atomic.Int64
	draining atomic.Bool
}

func newVersionedPool[T any](key PoolKey, plugins []T, maxProcs int) *VersionedPool[T] {
	size := maxProcs
	if size < len(plugins) {
		size = len(plugins)
	}
	p := &VersionedPool[T]{
		key:   key,
		slots: make(chan T, size),
	}
	for _, plugin := range plugins {
		p.slots <- plugin
	}
	return p
}

// Returns a plugin handle for exclusive use. Blocks until one is available or ctx is cancelled.
// Returns an error if the pool is draining.
func (p *VersionedPool[T]) Acquire(ctx context.Context) (T, error) {
	if p.draining.Load() {
		var zero T
		return zero, fmt.Errorf("pool %s is draining", p.key)
	}
	select {
	case plugin := <-p.slots:
		p.inflight.Add(1)
		return plugin, nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Release returns the handle to the pool after use.
func (p *VersionedPool[T]) Release(plugin T) {
	p.inflight.Add(-1)
	p.slots <- plugin
}

// Inflight returns the number of calls currently executing in this pool.
func (p *VersionedPool[T]) Inflight() int64 {
	return p.inflight.Load()
}

// Size returns the total capacity of this pool.
func (p *VersionedPool[T]) Size() int {
	return cap(p.slots)
}

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
		pp.metrics.poolSize.WithLabelValues(key.Id, key.Version).Set(float64(pool.Size()))
	}

	// Clear tombstone: plugin has come back (re-deployed after deletion).
	delete(pp.removed, key.Id)

	mode, _ := pp.routing(key.Id)

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

// DefaultCanaryHashKey is the call-site key used for consistent-hash canary routing.
var DefaultCanaryHashKey = "tenant_id"

// Acquires a handle from the appropriate pool (respecting canary/blue-green routing),
// invokes fn on it, and releases the handle.
//
// For shadow mode, only the production pool is called. Use CallWithShadow to also evaluate a shadow pool concurrently with a separate, independent closure.
func (pp *ProcessPool[T]) Call(ctx context.Context, id, hashKey string, fn func(context.Context, T) error) error {
	return pp.CallWithShadow(ctx, id, hashKey, fn, nil)
}

// CallWithShadow is like Call but also invokes shadowFn on the shadow pool concurrently
// when routing returns shadow mode for this plugin. shadowFn must operate on independent
// state (e.g. a cloned input, a separate result variable) to avoid data races with prodFn.
// Shadow errors are logged and counted but do not affect the return value.
func (pp *ProcessPool[T]) CallWithShadow(ctx context.Context, id, hashKey string, prodFn, shadowFn func(context.Context, T) error) error {
	// Snapshot everything we need under a short read lock.
	// User code (prodFn/shadowFn) is called after the lock is released.
	pp.mu.RLock()
	key, ok := pp.active[id]
	if !ok {
		_, removed := pp.removed[id]
		pp.mu.RUnlock()
		if removed {
			return fmt.Errorf("%w: %s", ErrPluginRemoved, id)
		}
		return fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	mode, rolloutPct := pp.routing(id)
	prodPool := pp.pools[key]
	// For canary/shadow: find any registered non-active pool for the same pluginID.
	var altPool *VersionedPool[T]
	if mode == RolloutModeCanary || mode == RolloutModeShadow {
		for k, p := range pp.pools {
			if k.Id == id && k != key {
				altPool = p
				break
			}
		}
	}
	pp.mu.RUnlock()

	if prodPool == nil {
		return fmt.Errorf("processpool: pool %s not found", key)
	}

	switch mode {
	case RolloutModeCanary:
		return pp.callCanary(ctx, id, hashKey, rolloutPct, prodPool, altPool, prodFn)
	case RolloutModeShadow:
		return pp.callShadow(ctx, id, prodPool, altPool, prodFn, shadowFn)
	}
	return pp.callPool(ctx, prodPool, prodFn)
}

// callCanary routes rolloutPct% of calls (via consistent hash on hashKey) to altPool
// when one exists. Pool pointers are pre-snapshotted by the caller under RLock.
func (pp *ProcessPool[T]) callCanary(ctx context.Context, id string, hashKey string, rolloutPct float64, prodPool, altPool *VersionedPool[T], fn func(context.Context, T) error) error {
	if hashKey == "" {
		hashKey = DefaultCanaryHashKey
	}
	h := fnv.New32a()
	h.Write([]byte(hashKey))
	pct := float64(h.Sum32()%100) + 1 // 1–100

	if pct <= rolloutPct && altPool != nil {
		return pp.callPool(ctx, altPool, fn)
	}
	return pp.callPool(ctx, prodPool, fn)
}

// callShadow calls prodFn on the production pool, then fires shadowFn on altPool
// in a background goroutine. Pool pointers are pre-snapshotted by the caller under RLock.
func (pp *ProcessPool[T]) callShadow(ctx context.Context, id string, prodPool, altPool *VersionedPool[T], prodFn, shadowFn func(context.Context, T) error) error {
	prodErr := pp.callPool(ctx, prodPool, prodFn)

	if shadowFn != nil && altPool != nil {
		// Detach from the caller's context: the production call has already returned,
		// so the caller's deadline may have expired or the ctx may be cancelled before
		// the shadow goroutine gets CPU time. Shadow evaluation must be independent.
		shadowCtx := context.WithoutCancel(ctx)
		shadowPool := altPool
		go func() {
			plugin, err := shadowPool.Acquire(shadowCtx)
			if err != nil {
				log.Printf("processpool: shadow acquire failed for %s: %v", id, err)
				return
			}
			defer shadowPool.Release(plugin)
			if err := shadowFn(shadowCtx, plugin); err != nil {
				log.Printf("processpool: shadow error for %s: %v", id, err)
				if pp.metrics != nil {
					pp.metrics.shadowDiffs.WithLabelValues(id).Inc()
				}
			}
		}()
	}

	return prodErr
}

func (pp *ProcessPool[T]) callPool(ctx context.Context, pool *VersionedPool[T], fn func(context.Context, T) error) error {
	plugin, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer pool.Release(plugin)
	return fn(ctx, plugin)
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
		pp.metrics.drainDuration.WithLabelValues(key.Id, key.Version).Observe(elapsed)
		pp.metrics.poolSize.WithLabelValues(key.Id, key.Version).Set(0)
		pp.metrics.poolInflight.WithLabelValues(key.Id, key.Version).Set(0)
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

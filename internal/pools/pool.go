package pools

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sync/atomic"
	"time"
)

// Returned by the pool when a plugin's KillSwitch is true.
var ErrKillSwitched = errors.New("plugin kill-switched")

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
// Return zero values for default blue-green behaviour (no kill switch, no canary).
type RoutingConfig func(pluginID string) (killSwitch bool, mode RolloutMode, rolloutPct float64)

// PoolKey uniquely identifies a versioned plugin subprocess pool.
type PoolKey struct {
	PluginID string
	Version  string
}

func (k PoolKey) String() string {
	return k.PluginID + "@" + k.Version
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

// Manages VersionedPools keyed by (PluginID, Version).
type ProcessPool[T any] struct {
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
	pool := newVersionedPool(key, plugins, maxProcs)
	pp.pools[key] = pool
	if pp.metrics != nil {
		pp.metrics.poolSize.WithLabelValues(key.PluginID, key.Version).Set(float64(pool.Size()))
	}

	// Clear tombstone: plugin has come back (re-deployed after deletion).
	delete(pp.removed, key.PluginID)

	_, mode, _ := pp.routing(key.PluginID)

	if mode == RolloutModeCanary || mode == RolloutModeShadow {
		// Stage the new pool without promoting - preserve active as production.
		// First registration for this pluginID still needs an active entry.
		if _, hasActive := pp.active[key.PluginID]; !hasActive {
			pp.active[key.PluginID] = key
		} else {
			// Drain the previous pending pool before replacing it so its subprocess
			// is killed and its onDrained callback fires. Without this, rapid deploys
			// in canary mode would orphan intermediate pools in pp.pools indefinitely.
			if prev, ok := pp.pending[key.PluginID]; ok {
				if prevPool, ok := pp.pools[prev.key]; ok {
					go pp.drain(prev.key, prevPool, prev.onDrained)
				}
			}
			pp.pending[key.PluginID] = pendingPromotion{key: key, onDrained: onDrained}
		}
		return
	}

	// Blue-green: promote immediately and drain old.
	oldKey, hasOld := pp.active[key.PluginID]
	pp.active[key.PluginID] = key

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
	p, ok := pp.pending[pluginID]
	if !ok {
		return
	}
	delete(pp.pending, pluginID)

	oldKey, hasOld := pp.active[pluginID]
	pp.active[pluginID] = p.key

	if hasOld && oldKey != p.key {
		if oldPool, ok := pp.pools[oldKey]; ok {
			go pp.drain(oldKey, oldPool, p.onDrained)
		}
	} else if p.onDrained != nil {
		p.onDrained()
	}
}

// Unregister removes the active pool for pluginID and drains it asynchronously. Any pending canary/shadow pool for the same pluginID is also drained.
// Used for transient stops (crash restarts, config disables) - no tombstone is set. Subsequent Call invocations return ErrPluginNotFound until the plugin re-registers.
func (pp *ProcessPool[T]) Unregister(pluginID string) {
	if p, ok := pp.pending[pluginID]; ok {
		delete(pp.pending, pluginID)
		if pool, ok := pp.pools[p.key]; ok {
			go pp.drain(p.key, pool, p.onDrained)
		}
	}
	key, ok := pp.active[pluginID]
	if !ok {
		return
	}
	delete(pp.active, pluginID)
	if pool, ok := pp.pools[key]; ok {
		go pp.drain(key, pool, nil)
	}
}

// Remove removes the active pool for pluginID, drains it asynchronously, and tombstones the plugin ID. Any pending canary/shadow pool is also drained.
// Used when a binary is permanently deleted from disk. Subsequent Call invocations return ErrPluginRemoved.
func (pp *ProcessPool[T]) Remove(pluginID string) {
	if p, ok := pp.pending[pluginID]; ok {
		delete(pp.pending, pluginID)
		if pool, ok := pp.pools[p.key]; ok {
			go pp.drain(p.key, pool, p.onDrained)
		}
	}
	key, ok := pp.active[pluginID]
	if !ok {
		pp.removed[pluginID] = struct{}{}
		return
	}
	delete(pp.active, pluginID)
	pp.removed[pluginID] = struct{}{}
	if pool, ok := pp.pools[key]; ok {
		go pp.drain(key, pool, nil)
	}
}

// DefaultCanaryHashKey is the call-site key used for consistent-hash canary routing.
var DefaultCanaryHashKey = "tenant_id"

// Acquires a handle from the appropriate pool (respecting kill-switch and
// canary/blue-green routing), invokes fn on it, and releases the handle.
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
	if err := pp.checkKillSwitch(id); err != nil {
		return err
	}

	key, ok := pp.active[id]
	if !ok {
		if _, removed := pp.removed[id]; removed {
			return fmt.Errorf("%w: %s", ErrPluginRemoved, id)
		}
		return fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}

	_, mode, rolloutPct := pp.routing(id)
	switch mode {
	case RolloutModeCanary:
		return pp.callCanary(ctx, key, id, hashKey, rolloutPct, prodFn)
	case RolloutModeShadow:
		return pp.callShadow(ctx, key, id, prodFn, shadowFn)
	}

	pool, ok := pp.pools[key]
	if !ok {
		return fmt.Errorf("processpool: pool %s not found", key)
	}
	return pp.callPool(ctx, pool, prodFn)
}

func (pp *ProcessPool[T]) checkKillSwitch(id string) error {
	killSwitch, _, _ := pp.routing(id)
	if killSwitch {
		if pp.metrics != nil {
			pp.metrics.killSwitches.WithLabelValues(id).Inc()
		}
		return fmt.Errorf("%w: %s", ErrKillSwitched, id)
	}
	return nil
}

// callCanary routes rolloutPct% of calls (via consistent hash on hashKey) to any
// non-active pool for the same pluginID. Remaining calls go to the production (active) pool.
func (pp *ProcessPool[T]) callCanary(ctx context.Context, prodKey PoolKey, id, hashKey string, rolloutPct float64, fn func(context.Context, T) error) error {
	if hashKey == "" {
		hashKey = DefaultCanaryHashKey
	}
	h := fnv.New32a()
	h.Write([]byte(hashKey))
	pct := float64(h.Sum32()%100) + 1 // 1–100

	if pct <= rolloutPct {
		// Find a registered non-active pool for the same pluginID.
		for k, pool := range pp.pools {
			if k.PluginID == id && k != prodKey {
				return pp.callPool(ctx, pool, fn)
			}
		}
	}

	prodPool, ok := pp.pools[prodKey]
	if !ok {
		return fmt.Errorf("processpool: production pool %s not found", prodKey)
	}
	return pp.callPool(ctx, prodPool, fn)
}

// callShadow calls prodFn on the production pool, then fires shadowFn on any
// non-active pool for the same pluginID in a background goroutine.
func (pp *ProcessPool[T]) callShadow(ctx context.Context, prodKey PoolKey, id string, prodFn, shadowFn func(context.Context, T) error) error {
	prodPool, ok := pp.pools[prodKey]
	if !ok {
		return fmt.Errorf("processpool: production pool %s not found", prodKey)
	}

	prodErr := pp.callPool(ctx, prodPool, prodFn)

	if shadowFn != nil {
		// Find a registered non-active pool for the same pluginID.
		for k, sp := range pp.pools {
			if k.PluginID == id && k != prodKey {
				shadowPool := sp
				go func() {
					plugin, err := shadowPool.Acquire(ctx)
					if err != nil {
						log.Printf("processpool: shadow acquire failed for %s: %v", id, err)
						return
					}
					defer shadowPool.Release(plugin)
					if err := shadowFn(ctx, plugin); err != nil {
						log.Printf("processpool: shadow error for %s: %v", id, err)
						if pp.metrics != nil {
							pp.metrics.shadowDiffs.WithLabelValues(id).Inc()
						}
					}
				}()
				break
			}
		}
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
		pp.metrics.drainDuration.WithLabelValues(key.PluginID, key.Version).Observe(elapsed)
		pp.metrics.poolSize.WithLabelValues(key.PluginID, key.Version).Set(0)
		pp.metrics.poolInflight.WithLabelValues(key.PluginID, key.Version).Set(0)
	}

	if pool.Inflight() > 0 {
		log.Printf("processpool: force-killed pool %s after %.1fs drain (%d in-flight)", key, elapsed, pool.Inflight())
	} else {
		log.Printf("processpool: drained pool %s in %.2fs", key, elapsed)
	}
	delete(pp.pools, key)

	if onDrained != nil {
		onDrained()
	}
}

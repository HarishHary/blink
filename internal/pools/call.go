package pools

import (
	"context"
	"fmt"
	"log"
)

// Call serves the caller's result from the routed pool; CallShadow mirrors to the shadow candidate in the
// background (dropped). All rollout-mode logic lives here, so the plugin pools stay mode-agnostic:
//
//	blue-green -> Call: stable  |  canary -> Call: candidate for the hashed slice, else stable
//	shadow     -> Call: stable  +  CallShadow: candidate (dropped)
func (pp *ProcessPool[T]) Call(ctx context.Context, id, rolloutKey string, fn func(context.Context, T) error) error {
	// Snapshot state under a short RLock; user code runs after release so plugin latency never blocks mutations.
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
	mode, rolloutPct := pp.rolloutCfg(id, "")
	prodPool := pp.pools[key]
	var altPool *VersionedPool[T]
	if mode == RolloutModeCanary {
		altPool = pp.findAltPool(id)
	}
	pp.mu.RUnlock()

	if prodPool == nil {
		return fmt.Errorf("processpool: pool %s not found", key)
	}
	if mode == RolloutModeCanary {
		candidatePool := servingPool(rolloutKey, rolloutPct, prodPool, altPool)
		return pp.callPool(ctx, candidatePool, fn)
	}
	return pp.callPool(ctx, prodPool, fn)
}

// CallShadow runs fn against id's shadow candidate in a detached goroutine (result dropped, errors logged).
// No-op unless id is in shadow mode with a registered candidate.
func (pp *ProcessPool[T]) CallShadow(ctx context.Context, id string, fn func(context.Context, T) error) {
	pp.mu.RLock()
	mode, _ := pp.rolloutCfg(id, "")
	var altPool *VersionedPool[T]
	if mode == RolloutModeShadow {
		altPool = pp.findAltPool(id)
	}
	pp.mu.RUnlock()
	if altPool == nil {
		return
	}
	// Detach from caller cancellation, but retain its deadline so the shadow call
	// cannot outlive the bounded production attempt that spawned it.
	deadline, hasDeadline := ctx.Deadline()
	shadowCtx := context.WithoutCancel(ctx)
	var cancel context.CancelFunc
	if hasDeadline {
		shadowCtx, cancel = context.WithDeadline(shadowCtx, deadline)
	}
	go func() {
		if cancel != nil {
			defer cancel()
		}
		plugin, err := altPool.Acquire(shadowCtx)
		if err != nil {
			log.Printf("processpool: shadow acquire failed for %s: %v", id, err)
			return
		}
		defer altPool.Release(plugin)
		if err := fn(shadowCtx, plugin); err != nil {
			log.Printf("processpool: shadow error for %s: %v", id, err)
			if pp.metrics != nil {
				pp.metrics.shadowDiffs.WithLabelValues(id).Inc()
			}
		}
	}()
}

// findAltPool returns id's staged canary/shadow candidate pool (the pending one), or nil; caller holds pp.mu.
func (pp *ProcessPool[T]) findAltPool(id string) *VersionedPool[T] {
	if p, ok := pp.pending[id]; ok {
		return pp.pools[p.key]
	}
	return nil
}

// servingPool returns altPool when rolloutKey hashes within rolloutPct (and altPool exists), else prodPool.
// Single source of the canary decision, shared by Call and ServingPoolSize so the two never disagree.
func servingPool[T any](rolloutKey string, rolloutPct float64, prodPool, altPool *VersionedPool[T]) *VersionedPool[T] {
	if float64(RolloutBucket(rolloutKey)) <= rolloutPct && altPool != nil {
		return altPool
	}
	return prodPool
}

// callPool acquires a handle from pool, runs fn, then releases it.
func (pp *ProcessPool[T]) callPool(ctx context.Context, pool *VersionedPool[T], fn func(context.Context, T) error) error {
	plugin, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer pool.Release(plugin)
	return fn(ctx, plugin)
}

// ServingPoolSize returns the max_procs of the pool that serves a batch for id (0 if none): stable for
// blue-green/shadow, or the hash-routed pool for canary. Sizes the production fan-out.
//
// ponytail: this and the real Call resolve rollout under separate RLocks, so a Promote/Register landing
// in between can make K track a pool the batch no longer routes to. Harmless - K only sets shard count,
// so a transient mismatch just over/under-shards one batch and self-corrects on the next.
func (pp *ProcessPool[T]) ServingPoolSize(id, rolloutKey string) int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	key, ok := pp.active[id]
	if !ok {
		return 0
	}
	stablePool := pp.pools[key]
	pool := stablePool
	if mode, pct := pp.rolloutCfg(id, ""); mode == RolloutModeCanary {
		altPool := pp.findAltPool(id)
		if altPool == nil {
			return 0
		}
		pool = servingPool(rolloutKey, pct, stablePool, altPool)
	}
	if pool == nil {
		return 0
	}
	return pool.Size()
}

// ShadowPoolSize returns the shadow candidate's max_procs when id is in shadow mode (else 0); sizes the shadow fan-out.
func (pp *ProcessPool[T]) ShadowPoolSize(id string) int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	mode, _ := pp.rolloutCfg(id, "")
	if mode == RolloutModeShadow {
		if altPool := pp.findAltPool(id); altPool != nil {
			return altPool.Size()
		}
	}
	return 0
}

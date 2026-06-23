package pools

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
)

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
func (pp *ProcessPool[T]) CallWithShadow(ctx context.Context, id string, hashKey string, prodFn, shadowFn func(context.Context, T) error) error {
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
	mode, rolloutPct := pp.routing(id, "")
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

package pools

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
)

// DefaultCanaryHashKey is the call-site key used for consistent-hash canary routing.
var DefaultCanaryHashKey = "tenant_id"

// Call acquires a handle from the routed pool, runs fn, releases it. Shadow mode hits only production - use CallWithShadow for a concurrent shadow closure.
func (pp *ProcessPool[T]) Call(ctx context.Context, id, hashKey string, fn func(context.Context, T) error) error {
	return pp.CallWithShadow(ctx, id, hashKey, fn, nil)
}

// CallWithShadow is Call plus a concurrent shadowFn on the shadow pool (shadow mode). shadowFn must use independent state (cloned input/result) to avoid races; shadow errors are logged/counted, not returned.
func (pp *ProcessPool[T]) CallWithShadow(ctx context.Context, id string, hashKey string, prodFn, shadowFn func(context.Context, T) error) error {
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

// callCanary routes rolloutPct% of calls (consistent hash on hashKey) to altPool when set; pools pre-snapshotted under RLock.
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

// callShadow runs prodFn on production, then fires shadowFn on altPool in a background goroutine; pools pre-snapshotted under RLock.
func (pp *ProcessPool[T]) callShadow(ctx context.Context, id string, prodPool, altPool *VersionedPool[T], prodFn, shadowFn func(context.Context, T) error) error {
	prodErr := pp.callPool(ctx, prodPool, prodFn)

	if shadowFn != nil && altPool != nil {
		// Detach from caller ctx: prod already returned, so its deadline may expire before the shadow goroutine runs.
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

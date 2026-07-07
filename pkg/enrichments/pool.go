package enrichments

import (
	"context"
	stderrors "errors"
	"maps"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
)

type Pool struct {
	*pools.ProcessPool[Enrichment]
}

// NewPool builds the enrichment pool with live rollout derived from cfg (see the
// rules pool for the closure rationale).
func NewPool(cfg config.Source[*EnrichmentMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Enrichment](config.RolloutFor(cfg), pools.NewPoolMetrics("enrichments"), drainTimeout),
	}
}

type enrichChunkResult struct {
	absent  bool
	removed bool
	errs    []errors.Error // per-alert (aligned with the chunk)
	callErr errors.Error   // whole-call failure (not per-alert)
}

// Enrich calls enrichmentID with all alerts. When the routed pool has max_procs > 1 the alerts are
// sharded into that many contiguous chunks enriched concurrently (each on its own subprocess); the
// chunks are DISJOINT, so no two shards ever touch the same *Alert. Per-alert errs are concatenated
// in original order. absent/removed refer to the plugin state.
func (p *Pool) Enrich(ctx context.Context, enrichmentID string, alts []*alerts.Alert, canaryHashKey string) (absent bool, removed bool, errs []errors.Error) {
	// Shadow candidate (if any): separate fan-out at its own max_procs on CLONED alerts (Enrich writes),
	// fired before the production path so the clone reads complete before prod mutates. Results dropped.
	p.shadowEnrich(ctx, enrichmentID, alts)
	k := p.ServingPoolSize(enrichmentID, canaryHashKey)
	parts := pools.ShardConcurrent(alts, k, func(altsChunk []*alerts.Alert) enrichChunkResult {
		return p.enrichChunk(ctx, enrichmentID, altsChunk, canaryHashKey)
	})

	// Pool-level conditions apply to the whole batch (every shard hit the same routed pool).
	for _, part := range parts {
		if part.removed {
			return false, true, nil
		}
		if part.absent {
			return true, false, nil
		}
		if part.callErr != nil {
			return false, false, []errors.Error{part.callErr}
		}
	}

	errs = make([]errors.Error, 0, len(alts))
	for _, part := range parts {
		errs = append(errs, part.errs...)
	}
	return false, false, errs
}

func (p *Pool) enrichChunk(ctx context.Context, enrichmentID string, altsChunk []*alerts.Alert, canaryHashKey string) enrichChunkResult {
	perErrs := make([]errors.Error, len(altsChunk))
	prodFn := func(callCtx context.Context, e Enrichment) error {
		if !e.EnrichmentMetadata().Enabled {
			return nil
		}
		if err := e.Enrich(callCtx, altsChunk); err != nil {
			for i := range perErrs {
				perErrs[i] = errors.NewE(err)
			}
		}
		return nil
	}
	err := p.Call(ctx, enrichmentID, canaryHashKey, prodFn)
	if err != nil {
		if stderrors.Is(err, pools.ErrPluginNotFound) {
			return enrichChunkResult{absent: true}
		}
		if stderrors.Is(err, pools.ErrPluginRemoved) {
			return enrichChunkResult{removed: true}
		}
		return enrichChunkResult{callErr: errors.NewE(err)}
	}
	return enrichChunkResult{errs: perErrs}
}

// shadowEnrich fans the full batch out to the shadow candidate (if enrichmentID is in shadow mode) at
// its own max_procs, each shard a detached CallShadow. Enrich WRITES the alerts, so each shard runs on
// its own cloned copies (cloneAlerts); the clones are taken synchronously here, before the production
// path mutates the batch. Results dropped.
func (p *Pool) shadowEnrich(ctx context.Context, enrichmentID string, alts []*alerts.Alert) {
	sk := p.ShadowPoolSize(enrichmentID)
	if sk == 0 || len(alts) == 0 {
		return
	}
	for _, chunk := range pools.ShardSlice(alts, sk) {
		shadowAlerts := cloneAlerts(chunk)
		p.CallShadow(ctx, enrichmentID, func(callCtx context.Context, e Enrichment) error {
			if !e.EnrichmentMetadata().Enabled {
				return nil
			}
			_ = e.Enrich(callCtx, shadowAlerts)
			return nil
		})
	}
}

// cloneAlerts returns shallow alert copies with cloned Event maps, so a shadow Enrich (which
// writes Event) mutates its own copies and never the batch the production call is enriching.
func cloneAlerts(in []*alerts.Alert) []*alerts.Alert {
	out := make([]*alerts.Alert, len(in))
	for i, a := range in {
		cp := *a
		cp.Event = maps.Clone(cp.Event)
		out[i] = &cp
	}
	return out
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

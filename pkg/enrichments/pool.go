package enrichments

import (
	"context"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	alts "github.com/harishhary/blink/pkg/alerts"
)

// Pool routes enrichment calls across the live enrichment process pool.
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

// EnrichResult holds the batch-level result from enriching alerts.
type EnrichResult struct {
	Errs    []errors.Error // per-alert (aligned with the input alerts)
	CallErr errors.Error   // whole-call failure; never alert-scoped
}

// Enrich calls enrichmentID with all alerts. When the routed pool has max_procs > 1 the alerts are
// sharded into that many contiguous chunks enriched concurrently (each on its own subprocess); the
// chunks are DISJOINT, so no two shards ever touch the same *Alert. Per-alert errs are concatenated
// in original order.
func (p *Pool) Enrich(ctx context.Context, enrichmentID string, alerts []*alts.Alert) EnrichResult {
	// Shadow candidate (if any): separate fan-out at its own max_procs on CLONED alerts (Enrich writes),
	// fired before the production path so the clone reads complete before prod mutates. Results dropped.
	p.shadowEnrich(ctx, enrichmentID, alerts)

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		alerts     []*alts.Alert
	}
	groups := make([]routeGroup, 0)
	groupIndexByBucket := make(map[uint32]int)
	for i, alert := range alerts {
		rolloutKey := pools.NormalizeRolloutKey(alert.Event["tenant_id"])
		bucket := pools.RolloutBucket(rolloutKey)
		groupIndex, ok := groupIndexByBucket[bucket]
		if !ok {
			groupIndex = len(groups)
			groupIndexByBucket[bucket] = groupIndex
			groups = append(groups, routeGroup{rolloutKey: rolloutKey})
		}
		groups[groupIndex].indexes = append(groups[groupIndex].indexes, i)
		groups[groupIndex].alerts = append(groups[groupIndex].alerts, alert)
	}

	parts := make([]EnrichResult, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := p.ServingPoolSize(enrichmentID, group.rolloutKey)
			shards := pools.ShardConcurrent(group.alerts, k, func(alertChunk []*alts.Alert) EnrichResult {
				return p.enrichChunk(ctx, enrichmentID, alertChunk, group.rolloutKey)
			})
			part := EnrichResult{Errs: make([]errors.Error, 0, len(group.alerts))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = EnrichResult{CallErr: shard.CallErr}
					return
				}
				part.Errs = append(part.Errs, shard.Errs...)
			}
			parts[i] = part
		}(i)
	}
	wg.Wait()

	result := EnrichResult{Errs: make([]errors.Error, len(alerts))}
	for i, part := range parts {
		if part.CallErr != nil {
			return EnrichResult{CallErr: part.CallErr}
		}
		group := groups[i]
		if len(part.Errs) != len(group.indexes) {
			return EnrichResult{CallErr: errors.NewF("enrichment %s returned invalid routed result shape", enrichmentID)}
		}
		for j, inputIndex := range group.indexes {
			result.Errs[inputIndex] = part.Errs[j]
		}
	}
	return result
}

func (p *Pool) enrichChunk(ctx context.Context, enrichmentID string, altsChunk []*alts.Alert, rolloutKey string) EnrichResult {
	perErrs := make([]errors.Error, len(altsChunk))
	prodFn := func(callCtx context.Context, e Enrichment) error {
		if !e.EnrichmentMetadata().Enabled {
			return nil
		}
		batchResult := e.EnrichBatch(callCtx, altsChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Errs) != len(altsChunk) {
			return &errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: enrichmentID, Field: "errors", Expected: len(altsChunk), Actual: len(batchResult.Errs)}
		}
		copy(perErrs, batchResult.Errs)
		return nil
	}
	err := p.Call(ctx, enrichmentID, rolloutKey, prodFn)
	if err != nil {
		return EnrichResult{CallErr: errors.NewE(err)}
	}
	return EnrichResult{Errs: perErrs}
}

// shadowEnrich fans the full batch out to the shadow candidate (if enrichmentID is in shadow mode) at
// its own max_procs, each shard a detached CallShadow. Enrich WRITES the alerts, so each shard runs on
// its own cloned copies (cloneAlerts); the clones are taken synchronously here, before the production
// path mutates the batch. Results dropped.
func (p *Pool) shadowEnrich(ctx context.Context, enrichmentID string, alerts []*alts.Alert) {
	sk := p.ShadowPoolSize(enrichmentID)
	if sk == 0 || len(alerts) == 0 {
		return
	}
	for _, altsChunk := range pools.ShardSlice(alerts, sk) {
		shadowAlerts := alts.CloneBatch(altsChunk)
		p.CallShadow(ctx, enrichmentID, func(callCtx context.Context, e Enrichment) error {
			if !e.EnrichmentMetadata().Enabled {
				return nil
			}
			result := e.EnrichBatch(callCtx, shadowAlerts)
			if result.CallErr != nil {
				return result.CallErr
			}
			for _, err := range result.Errs {
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

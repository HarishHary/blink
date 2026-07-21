package formatters

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

// Pool routes formatter calls across the live formatter process pool.
type Pool struct {
	*pools.ProcessPool[Formatter]
}

// NewPool builds the formatter pool with live rollout derived from cfg (see the
// rules pool for the closure rationale).
func NewPool(cfg config.Source[*FormatterMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Formatter](config.RolloutFor(cfg), pools.NewPoolMetrics("formatters"), drainTimeout),
	}
}

// FormatItem holds one alert's formatting outcome.
type FormatItem struct {
	Output map[string]any
	Err    errors.Error
}

// FormatResult holds the batch-level result from formatting alerts.
type FormatResult struct {
	Items   []FormatItem
	CallErr errors.Error // whole-call failure; never alert-scoped
}

// Format runs the formatter identified by id against all alerts. When the routed pool has
// max_procs > 1 the alerts are sharded into that many contiguous chunks formatted concurrently
// (each on its own subprocess) and the per-alert items are concatenated in original order.
func (p *Pool) Format(ctx context.Context, formatterID string, alerts []*alts.Alert) FormatResult {
	p.shadowFormat(ctx, formatterID, alerts)

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		alerts     []*alts.Alert
	}
	groups := make([]routeGroup, 0)
	groupIndexByBucket := make(map[uint32]int)
	for i, alert := range alerts {
		rolloutKey := pools.TenantRolloutKey(alert.Event["tenant_id"])
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

	parts := make([]FormatResult, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := p.ServingPoolSize(formatterID, group.rolloutKey)
			shards := pools.ShardConcurrent(group.alerts, k, func(alertChunk []*alts.Alert) FormatResult {
				return p.formatChunk(ctx, formatterID, alertChunk, group.rolloutKey)
			})
			part := FormatResult{Items: make([]FormatItem, 0, len(group.alerts))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = FormatResult{CallErr: shard.CallErr}
					return
				}
				part.Items = append(part.Items, shard.Items...)
			}
			parts[i] = part
		}(i)
	}
	wg.Wait()

	result := FormatResult{Items: make([]FormatItem, len(alerts))}
	for i, part := range parts {
		if part.CallErr != nil {
			return FormatResult{CallErr: part.CallErr}
		}
		group := groups[i]
		if len(part.Items) != len(group.indexes) {
			return FormatResult{CallErr: errors.NewF("formatter %s returned invalid routed result shape", formatterID)}
		}
		for j, inputIndex := range group.indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

func (p *Pool) formatChunk(ctx context.Context, formatterID string, altsChunk []*alts.Alert, rolloutKey string) FormatResult {
	items := make([]FormatItem, len(altsChunk))
	prodFn := func(callCtx context.Context, f Formatter) error {
		if !f.FormatterMetadata().Enabled {
			return nil
		}
		batchResult := f.FormatBatch(callCtx, altsChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Items) != len(altsChunk) {
			return &errors.ResultCardinalityError{PluginKind: "formatter", PluginID: formatterID, Field: "items", Expected: len(altsChunk), Actual: len(batchResult.Items)}
		}
		copy(items, batchResult.Items)
		return nil
	}
	err := p.Call(ctx, formatterID, rolloutKey, prodFn)
	if err != nil {
		return FormatResult{CallErr: errors.NewE(err)}
	}
	return FormatResult{Items: items}
}

// shadowFormat fans the full batch out to the shadow candidate (if formatterID is in shadow mode) at
// its own max_procs, each shard a detached CallShadow whose result is dropped. Format is read-only on
// the alerts (returns new maps), so shards share the batch.
func (p *Pool) shadowFormat(ctx context.Context, formatterID string, alerts []*alts.Alert) {
	sk := p.ShadowPoolSize(formatterID)
	if sk == 0 || len(alerts) == 0 {
		return
	}
	for _, altsChunk := range pools.ShardSlice(alerts, sk) {
		p.CallShadow(ctx, formatterID, func(callCtx context.Context, f Formatter) error {
			if !f.FormatterMetadata().Enabled {
				return nil
			}
			result := f.FormatBatch(callCtx, altsChunk)
			if result.CallErr != nil {
				return result.CallErr
			}
			for _, item := range result.Items {
				if item.Err != nil {
					return item.Err
				}
			}
			return nil
		})
	}
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

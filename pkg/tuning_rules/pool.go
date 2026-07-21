package tuning_rules

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
	"github.com/harishhary/blink/pkg/scoring"
)

// Pool is the tuning-rule process pool.
type Pool struct {
	*pools.ProcessPool[TuningRule]
}

// NewPool builds the tuning-rule pool with live rollout routing derived from cfg.
func NewPool(cfg config.Source[*TuningRuleMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[TuningRule](config.RolloutFor(cfg), pools.NewPoolMetrics("tuning_rules"), drainTimeout),
	}
}

// TuneItem holds one alert's tuning outcome and the metadata of the selected plugin version.
type TuneItem struct {
	RuleType   RuleType
	Confidence scoring.Confidence
	Applies    bool
	Err        errors.Error
}

// TuneResult holds the batch-level result from tuning alerts.
type TuneResult struct {
	Items   []TuneItem
	CallErr errors.Error // whole-call failure; never alert-scoped
}

// Tune runs tuningRuleID across the batch and returns metadata, per-alert apply results, and per-alert errors.
func (p *Pool) Tune(ctx context.Context, tuningRuleID string, alerts []alts.Alert) TuneResult {
	p.shadowTune(ctx, tuningRuleID, alerts)

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		alerts     []alts.Alert
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

	parts := make([]TuneResult, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := p.ServingPoolSize(tuningRuleID, group.rolloutKey)
			shards := pools.ShardConcurrent(group.alerts, k, func(alertChunk []alts.Alert) TuneResult {
				return p.tuneChunk(ctx, tuningRuleID, alertChunk, group.rolloutKey)
			})
			part := TuneResult{Items: make([]TuneItem, 0, len(group.alerts))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = TuneResult{CallErr: shard.CallErr}
					return
				}
				part.Items = append(part.Items, shard.Items...)
			}
			parts[i] = part
		}(i)
	}
	wg.Wait()

	result := TuneResult{Items: make([]TuneItem, len(alerts))}
	for i, part := range parts {
		if part.CallErr != nil {
			return TuneResult{CallErr: part.CallErr}
		}
		group := groups[i]
		if len(part.Items) != len(group.indexes) {
			return TuneResult{CallErr: errors.NewF("tuning rule %s returned invalid routed result shape", tuningRuleID)}
		}
		for j, inputIndex := range group.indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

func (p *Pool) tuneChunk(ctx context.Context, tuningRuleID string, alertChunk []alts.Alert, rolloutKey string) TuneResult {
	res := TuneResult{Items: make([]TuneItem, len(alertChunk))}
	prodFn := func(callCtx context.Context, t TuningRule) error {
		md := t.TuningRuleMetadata()
		if !md.Enabled {
			return nil
		}
		for i := range alertChunk {
			res.Items[i].RuleType = md.RuleType
			res.Items[i].Confidence = md.Confidence
		}
		batchResult := t.TuneBatch(callCtx, alertChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Items) != len(alertChunk) {
			return &errors.ResultCardinalityError{PluginKind: "tuning rule", PluginID: tuningRuleID, Field: "items", Expected: len(alertChunk), Actual: len(batchResult.Items)}
		}
		for i, item := range batchResult.Items {
			res.Items[i].Applies = item.Applies
			res.Items[i].Err = item.Err
		}
		return nil
	}
	err := p.Call(ctx, tuningRuleID, rolloutKey, prodFn)
	if err != nil {
		return TuneResult{CallErr: errors.NewE(err)}
	}
	return res
}

// shadowTune sends detached shadow traffic for comparison without affecting Tune's returned results.
func (p *Pool) shadowTune(ctx context.Context, tuningRuleID string, alerts []alts.Alert) {
	sk := p.ShadowPoolSize(tuningRuleID)
	if sk == 0 || len(alerts) == 0 {
		return
	}
	for _, alertChunk := range pools.ShardSlice(alerts, sk) {
		p.CallShadow(ctx, tuningRuleID, func(callCtx context.Context, t TuningRule) error {
			if !t.TuningRuleMetadata().Enabled {
				return nil
			}
			result := t.TuneBatch(callCtx, alertChunk)
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

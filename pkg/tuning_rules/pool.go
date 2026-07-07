package tuning_rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
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

type tuneChunkResult struct {
	ruleType   RuleType
	confidence scoring.Confidence
	applies    []bool
	err        errors.Error
}

// Tune runs tuningRuleID against all alerts, sharded across the pool's workers, returning per-alert
// apply results; ruleType/confidence (rule metadata, identical for all) come from the first shard.
func (p *Pool) Tune(ctx context.Context, tuningRuleID string, alertBatch []alerts.Alert, canaryHashKey string) (
	ruleType RuleType, confidence scoring.Confidence, applies []bool, _ errors.Error,
) {
	// Shadow candidate (if any): separate fan-out at its own max_procs, results dropped.
	p.shadowTune(ctx, tuningRuleID, alertBatch)
	k := p.ServingPoolSize(tuningRuleID, canaryHashKey)
	parts := pools.ShardConcurrent(alertBatch, k, func(alerts []alerts.Alert) tuneChunkResult {
		return p.tuneChunk(ctx, tuningRuleID, alerts, canaryHashKey)
	})

	applies = make([]bool, 0, len(alertBatch))
	for i, part := range parts {
		if part.err != nil {
			return 0, 0, nil, part.err
		}
		if i == 0 {
			ruleType = part.ruleType
			confidence = part.confidence
		}
		applies = append(applies, part.applies...)
	}
	return ruleType, confidence, applies, nil
}

func (p *Pool) tuneChunk(ctx context.Context, tuningRuleID string, alerts []alerts.Alert, canaryHashKey string) tuneChunkResult {
	var res tuneChunkResult
	res.applies = make([]bool, len(alerts))
	prodFn := func(callCtx context.Context, t TuningRule) error {
		md := t.TuningRuleMetadata()
		if !md.Enabled {
			return nil
		}
		res.ruleType = md.RuleType
		res.confidence = md.Confidence
		var e errors.Error
		res.applies, e = t.Tune(callCtx, alerts)
		return e
	}
	err := p.Call(ctx, tuningRuleID, canaryHashKey, prodFn)
	if err != nil {
		return tuneChunkResult{err: errors.NewE(err)}
	}
	return res
}

// shadowTune fans the full batch out to the shadow candidate (if tuningRuleID is in shadow mode) at its
// own max_procs, each shard a detached CallShadow whose result is dropped. Tune is read-only on the
// alerts (it returns decisions), so shards share the batch.
func (p *Pool) shadowTune(ctx context.Context, tuningRuleID string, alertBatch []alerts.Alert) {
	sk := p.ShadowPoolSize(tuningRuleID)
	if sk == 0 || len(alertBatch) == 0 {
		return
	}
	for _, alerts := range pools.ShardSlice(alertBatch, sk) {
		p.CallShadow(ctx, tuningRuleID, func(callCtx context.Context, t TuningRule) error {
			if !t.TuningRuleMetadata().Enabled {
				return nil
			}
			_, e := t.Tune(callCtx, alerts)
			return e
		})
	}
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

package tuning_rules

import (
	"context"
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

// TuneResult holds the batch-level result from tuning alerts.
type TuneResult struct {
	RuleType   RuleType
	Confidence scoring.Confidence
	Applies    []bool
	Errs       []errors.Error // per-alert (aligned with Applies)
	CallErr    errors.Error   // whole-call failure; never alert-scoped
}

// Tune runs tuningRuleID across the batch and returns metadata, per-alert apply results, and per-alert errors.
func (p *Pool) Tune(ctx context.Context, tuningRuleID string, alerts []alts.Alert, canaryHashKey string) TuneResult {
	p.shadowTune(ctx, tuningRuleID, alerts)
	k := p.ServingPoolSize(tuningRuleID, canaryHashKey)
	parts := pools.ShardConcurrent(alerts, k, func(alertChunk []alts.Alert) TuneResult {
		return p.tuneChunk(ctx, tuningRuleID, alertChunk, canaryHashKey)
	})
	result := TuneResult{
		Applies: make([]bool, 0, len(alerts)),
		Errs:    make([]errors.Error, 0, len(alerts)),
	}
	for i, part := range parts {
		if part.CallErr != nil {
			return TuneResult{CallErr: part.CallErr}
		}
		if i == 0 {
			result.RuleType = part.RuleType
			result.Confidence = part.Confidence
		}
		result.Applies = append(result.Applies, part.Applies...)
		result.Errs = append(result.Errs, part.Errs...)
	}
	return result
}

func (p *Pool) tuneChunk(ctx context.Context, tuningRuleID string, alertChunk []alts.Alert, canaryHashKey string) TuneResult {
	res := TuneResult{Applies: make([]bool, len(alertChunk)), Errs: make([]errors.Error, len(alertChunk))}
	prodFn := func(callCtx context.Context, t TuningRule) error {
		md := t.TuningRuleMetadata()
		if !md.Enabled {
			return nil
		}
		res.RuleType = md.RuleType
		res.Confidence = md.Confidence
		batchResult := t.TuneBatch(callCtx, alertChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Applies) != len(alertChunk) {
			return &errors.ResultCardinalityError{PluginKind: "tuning rule", PluginID: tuningRuleID, Field: "applies", Expected: len(alertChunk), Actual: len(batchResult.Applies)}
		}
		if len(batchResult.Errs) != len(alertChunk) {
			return &errors.ResultCardinalityError{PluginKind: "tuning rule", PluginID: tuningRuleID, Field: "errors", Expected: len(alertChunk), Actual: len(batchResult.Errs)}
		}
		copy(res.Applies, batchResult.Applies)
		copy(res.Errs, batchResult.Errs)
		return nil
	}
	err := p.Call(ctx, tuningRuleID, canaryHashKey, prodFn)
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

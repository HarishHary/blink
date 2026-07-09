package tuning_rules

import (
	"context"
	stderrors "errors"
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

type tuneChunkResult struct {
	ruleType   RuleType
	confidence scoring.Confidence
	applies    []bool
	absent     bool
	removed    bool
	errs       []errors.Error // per-alert (aligned with the chunk)
	callErr    errors.Error   // whole-call failure (not per-alert)
}

// TuneResult holds the batch-level result from tuning alerts.
type TuneResult struct {
	RuleType   RuleType
	Confidence scoring.Confidence
	Applies    []bool
	Absent     bool
	Removed    bool
	Errs       []errors.Error // per-alert (aligned with Applies)
}

// Tune runs tuningRuleID across the batch and returns metadata, per-alert apply results, pool state flags, and per-alert errors.
func (p *Pool) Tune(ctx context.Context, tuningRuleID string, alerts []alts.Alert, canaryHashKey string) TuneResult {
	p.shadowTune(ctx, tuningRuleID, alerts)
	k := p.ServingPoolSize(tuningRuleID, canaryHashKey)
	parts := pools.ShardConcurrent(alerts, k, func(altsChunk []alts.Alert) tuneChunkResult {
		return p.tuneChunk(ctx, tuningRuleID, altsChunk, canaryHashKey)
	})

	for _, part := range parts {
		if part.removed {
			return TuneResult{Removed: true}
		}
		if part.absent {
			return TuneResult{Absent: true}
		}
		if part.callErr != nil {
			for i := range part.errs {
				part.errs[i] = part.callErr
			}
		}
	}

	var ruleType RuleType
	var confidence scoring.Confidence
	applies := make([]bool, 0, len(alerts))
	errs := make([]errors.Error, 0, len(alerts))
	metadataSet := false
	for _, part := range parts {
		if !metadataSet && part.callErr == nil {
			ruleType = part.ruleType
			confidence = part.confidence
			metadataSet = true
		}
		applies = append(applies, part.applies...)
		errs = append(errs, part.errs...)
	}
	return TuneResult{RuleType: ruleType, Confidence: confidence, Applies: applies, Errs: errs}
}

func (p *Pool) tuneChunk(ctx context.Context, tuningRuleID string, altsChunk []alts.Alert, canaryHashKey string) tuneChunkResult {
	var res tuneChunkResult
	res.applies = make([]bool, len(altsChunk))
	res.errs = make([]errors.Error, len(altsChunk))
	prodFn := func(callCtx context.Context, t TuningRule) error {
		md := t.TuningRuleMetadata()
		if !md.Enabled {
			return nil
		}
		res.ruleType = md.RuleType
		res.confidence = md.Confidence
		batchApplies, e := t.Tune(callCtx, altsChunk)
		if e != nil {
			for i := range res.errs {
				res.errs[i] = e
			}
			return nil
		}
		if len(batchApplies) != len(altsChunk) {
			e := errors.NewF("tuning rule %s returned %d results for %d alerts", tuningRuleID, len(batchApplies), len(altsChunk))
			for i := range res.errs {
				res.errs[i] = e
			}
			return nil
		}
		copy(res.applies, batchApplies)
		return nil
	}
	err := p.Call(ctx, tuningRuleID, canaryHashKey, prodFn)
	if err != nil {
		if stderrors.Is(err, pools.ErrPluginNotFound) {
			return tuneChunkResult{absent: true}
		}
		if stderrors.Is(err, pools.ErrPluginRemoved) {
			return tuneChunkResult{removed: true}
		}
		return tuneChunkResult{applies: res.applies, errs: res.errs, callErr: errors.NewE(err)}
	}
	return res
}

// shadowTune sends detached shadow traffic for comparison without affecting Tune's returned results.
func (p *Pool) shadowTune(ctx context.Context, tuningRuleID string, alerts []alts.Alert) {
	sk := p.ShadowPoolSize(tuningRuleID)
	if sk == 0 || len(alerts) == 0 {
		return
	}
	for _, altsChunk := range pools.ShardSlice(alerts, sk) {
		p.CallShadow(ctx, tuningRuleID, func(callCtx context.Context, t TuningRule) error {
			if !t.TuningRuleMetadata().Enabled {
				return nil
			}
			_, e := t.Tune(callCtx, altsChunk)
			return e
		})
	}
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

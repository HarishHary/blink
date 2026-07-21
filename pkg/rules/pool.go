package rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	evts "github.com/harishhary/blink/pkg/events"
)

// Pool manages live rule plugin processes and request routing.
type Pool struct {
	*pools.ProcessPool[Rule]
}

// EvaluateResult holds the result from evaluating events.
type EvaluateResult struct {
	Results []EventResult
	Errs    []errors.Error // per-event (aligned with Results)
	CallErr errors.Error   // whole-call failure; never record-scoped
}

// NewPool builds a rule process pool.
func NewPool(cfg config.Source[*RuleMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Rule](config.RolloutFor(cfg), pools.NewPoolMetrics("rules"), drainTimeout),
	}
}

// Evaluate runs a batch of events against the named rule.
func (p *Pool) Evaluate(ctx context.Context, ruleID string, events []evts.Event, canaryHashKey string) EvaluateResult {
	p.shadowEvaluate(ctx, ruleID, events)
	k := p.ServingPoolSize(ruleID, canaryHashKey)
	parts := pools.ShardConcurrent(events, k, func(chunk []evts.Event) EvaluateResult {
		return p.evaluateChunk(ctx, ruleID, chunk, canaryHashKey)
	})
	result := EvaluateResult{
		Results: make([]EventResult, 0, len(events)),
		Errs:    make([]errors.Error, 0, len(events)),
	}
	for _, part := range parts {
		if part.CallErr != nil {
			return EvaluateResult{CallErr: part.CallErr}
		}
		result.Results = append(result.Results, part.Results...)
		result.Errs = append(result.Errs, part.Errs...)
	}
	return result
}

// evaluateChunk is one production pool call: it acquires a subprocess (stable, or the canary candidate
// for the hashed slice) and evaluates evts against ruleID. The shadow candidate is driven separately by
// shadowEvaluate.
func (p *Pool) evaluateChunk(ctx context.Context, ruleID string, eventsChunk []evts.Event, canaryHashKey string) EvaluateResult {
	results := make([]EventResult, len(eventsChunk))
	perErrs := make([]errors.Error, len(eventsChunk))
	prodFn := func(callCtx context.Context, r Rule) error {
		if !r.RuleMetadata().Enabled {
			return nil
		}
		batchResult := r.EvaluateBatch(callCtx, eventsChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Results) != len(eventsChunk) {
			return &errors.ResultCardinalityError{PluginKind: "rule", PluginID: ruleID, Field: "results", Expected: len(eventsChunk), Actual: len(batchResult.Results)}
		}
		if len(batchResult.Errs) != len(eventsChunk) {
			return &errors.ResultCardinalityError{PluginKind: "rule", PluginID: ruleID, Field: "errors", Expected: len(eventsChunk), Actual: len(batchResult.Errs)}
		}
		copy(results, batchResult.Results)
		copy(perErrs, batchResult.Errs)
		return nil
	}
	err := p.Call(ctx, ruleID, canaryHashKey, prodFn)
	if err != nil {
		return EvaluateResult{CallErr: errors.NewE(err)}
	}
	return EvaluateResult{Results: results, Errs: perErrs}
}

// shadowEvaluate fans the full batch out to the shadow candidate (if ruleID is in shadow mode) at the
// candidate's own max_procs, each shard a detached CallShadow whose result is dropped. No-op otherwise.
// Events are read-only (World B: the executor builds alerts on fresh maps), so shards share the batch.
func (p *Pool) shadowEvaluate(ctx context.Context, ruleID string, events []evts.Event) {
	sk := p.ShadowPoolSize(ruleID)
	if sk == 0 || len(events) == 0 {
		return
	}
	for _, evtsChunk := range pools.ShardSlice(events, sk) {
		p.CallShadow(ctx, ruleID, func(callCtx context.Context, r Rule) error {
			if !r.RuleMetadata().Enabled {
				return nil
			}
			result := r.EvaluateBatch(callCtx, evtsChunk)
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

// Sync applies plugin lifecycle messages to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

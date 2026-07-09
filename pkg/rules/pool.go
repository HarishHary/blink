package rules

import (
	"context"
	stderrors "errors"
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

type evaluateChunkResult struct {
	results []EventResult
	absent  bool
	removed bool
	errs    []errors.Error // per-event (aligned with the chunk)
	callErr errors.Error   // whole-call failure (not per-event)
}

// EvaluateResult holds the batch-level result from evaluating a rule.
type EvaluateResult struct {
	Results []EventResult
	Absent  bool
	Removed bool
	Errs    []errors.Error // per-event (aligned with Results)
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
	parts := pools.ShardConcurrent(events, k, func(chunk []evts.Event) evaluateChunkResult {
		return p.evaluateChunk(ctx, ruleID, chunk, canaryHashKey)
	})

	for _, part := range parts {
		if part.removed {
			return EvaluateResult{Removed: true}
		}
		if part.absent {
			return EvaluateResult{Absent: true}
		}
		if part.callErr != nil {
			for i := range part.errs {
				part.errs[i] = part.callErr
			}
		}
	}

	results := make([]EventResult, 0, len(events))
	errs := make([]errors.Error, 0, len(events))
	for _, part := range parts {
		results = append(results, part.results...)
		errs = append(errs, part.errs...)
	}
	return EvaluateResult{Results: results, Errs: errs}
}

// evaluateChunk is one production pool call: it acquires a subprocess (stable, or the canary candidate
// for the hashed slice) and evaluates evts against ruleID. The shadow candidate is driven separately by
// shadowEvaluate.
func (p *Pool) evaluateChunk(ctx context.Context, ruleID string, eventsChunk []evts.Event, canaryHashKey string) evaluateChunkResult {
	results := make([]EventResult, len(eventsChunk))
	perErrs := make([]errors.Error, len(eventsChunk))
	prodFn := func(callCtx context.Context, r Rule) error {
		if !r.RuleMetadata().Enabled {
			return nil
		}
		batchResults, e := r.Evaluate(callCtx, eventsChunk)
		if e != nil {
			for i := range perErrs {
				perErrs[i] = e
			}
			return nil
		}
		if len(batchResults) != len(eventsChunk) {
			e := errors.NewF("rule %s returned %d results for %d events", ruleID, len(batchResults), len(eventsChunk))
			for i := range perErrs {
				perErrs[i] = e
			}
			return nil
		}
		copy(results, batchResults)
		return nil
	}
	err := p.Call(ctx, ruleID, canaryHashKey, prodFn)
	if err != nil {
		if stderrors.Is(err, pools.ErrPluginNotFound) {
			return evaluateChunkResult{absent: true}
		}
		if stderrors.Is(err, pools.ErrPluginRemoved) {
			return evaluateChunkResult{removed: true}
		}
		return evaluateChunkResult{results: results, errs: perErrs, callErr: errors.NewE(err)}
	}
	return evaluateChunkResult{results: results, errs: perErrs}
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
			_, e := r.Evaluate(callCtx, evtsChunk)
			return e
		})
	}
}

// Sync applies plugin lifecycle messages to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

package matchers

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

// Pool routes matcher calls across the live matcher process pool.
type Pool struct {
	*pools.ProcessPool[Matcher]
}

// NewPool builds the matcher pool with live rollout derived from cfg: a
// running canary/shadow artifact's mode+pct comes from its own spec (by binary name);
// otherwise the merged per-ID rollout applies. Mirrors the rules pool.
func NewPool(cfg config.Source[*MatcherMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Matcher](config.RolloutFor(cfg), pools.NewPoolMetrics("matchers"), drainTimeout),
	}
}

// MatchResult holds the result from matching events.
type MatchResult struct {
	Results []bool
	Errs    []errors.Error // per-event (aligned with Results)
	CallErr errors.Error   // whole-call failure; never record-scoped
}

// Match runs matcher matcherID against every event and returns one bool per event in input order
// (a disabled matcher passes through as all-true). It is deliberately mode-agnostic - the rollout mode
// is handled entirely inside the pool primitives, so the body reads the same for every mode. Two things
// happen, and only the second depends on the mode:
//
//   - Production (all modes): the batch is sharded across the serving pool's workers and matched
//     concurrently. Call sends each shard to the stable version, or to a canary slice - Match does not
//     need to know which.
//   - Shadow (shadow mode only): shadowMatch also mirrors the whole batch to the candidate in the
//     background at the candidate's own max_procs, dropping the result. It is a no-op in every other mode.
func (p *Pool) Match(ctx context.Context, matcherID string, events []evts.Event, canaryHashKey string) MatchResult {
	p.shadowMatch(ctx, matcherID, events)
	k := p.ServingPoolSize(matcherID, canaryHashKey)
	parts := pools.ShardConcurrent(events, k, func(chunk []evts.Event) MatchResult {
		return p.matchChunk(ctx, matcherID, chunk, canaryHashKey)
	})
	result := MatchResult{
		Results: make([]bool, 0, len(events)),
		Errs:    make([]errors.Error, 0, len(events)),
	}
	for _, part := range parts {
		if part.CallErr != nil {
			return MatchResult{CallErr: part.CallErr}
		}
		result.Results = append(result.Results, part.Results...)
		result.Errs = append(result.Errs, part.Errs...)
	}
	return result
}

// matchChunk is one production call: it matches a single shard on the serving pool (stable, or the
// canary slice - decided inside Call). A disabled matcher returns all-true.
func (p *Pool) matchChunk(ctx context.Context, matcherID string, eventChunk []evts.Event, canaryHashKey string) MatchResult {
	results := make([]bool, len(eventChunk))
	perErrs := make([]errors.Error, len(eventChunk))
	prodFn := func(callCtx context.Context, m Matcher) error {
		if !m.MatcherMetadata().Enabled {
			for i := range results {
				results[i] = true
			}
			return nil
		}
		batchResult := m.MatchBatch(callCtx, eventChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Results) != len(eventChunk) {
			return &errors.ResultCardinalityError{PluginKind: "matcher", PluginID: matcherID, Field: "results", Expected: len(eventChunk), Actual: len(batchResult.Results)}
		}
		if len(batchResult.Errs) != len(eventChunk) {
			return &errors.ResultCardinalityError{PluginKind: "matcher", PluginID: matcherID, Field: "errors", Expected: len(eventChunk), Actual: len(batchResult.Errs)}
		}
		copy(results, batchResult.Results)
		copy(perErrs, batchResult.Errs)
		return nil
	}
	err := p.Call(ctx, matcherID, canaryHashKey, prodFn)
	if err != nil {
		return MatchResult{CallErr: errors.NewE(err)}
	}
	return MatchResult{Results: results, Errs: perErrs}
}

// shadowMatch fans the full batch out to the shadow candidate (if matcherID is in shadow mode) at its
// own max_procs, each shard a detached CallShadow whose result is dropped. Match is read-only on evts,
// so shards share the batch.
func (p *Pool) shadowMatch(ctx context.Context, matcherID string, events []evts.Event) {
	sk := p.ShadowPoolSize(matcherID)
	if sk == 0 || len(events) == 0 {
		return
	}
	for _, chunk := range pools.ShardSlice(events, sk) {
		p.CallShadow(ctx, matcherID, func(callCtx context.Context, m Matcher) error {
			if !m.MatcherMetadata().Enabled {
				return nil
			}
			result := m.MatchBatch(callCtx, chunk)
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

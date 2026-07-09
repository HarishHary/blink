package matchers

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

type matchChunkResult struct {
	result  []bool
	absent  bool
	removed bool
	errs    []errors.Error // per-event plugin Match failures, aligned with the chunk
	callErr errors.Error   // whole-call failure (not plugin Match result)
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
func (p *Pool) Match(ctx context.Context, matcherID string, events []evts.Event, canaryHashKey string) (results []bool, absent bool, removed bool, errs []errors.Error) {
	p.shadowMatch(ctx, matcherID, events)
	k := p.ServingPoolSize(matcherID, canaryHashKey)
	parts := pools.ShardConcurrent(events, k, func(chunk []evts.Event) matchChunkResult {
		return p.matchChunk(ctx, matcherID, chunk, canaryHashKey)
	})

	// Pool-level conditions apply to the whole batch (every shard hit the same routed pool).
	for _, part := range parts {
		if part.removed {
			return nil, false, true, nil
		}
		if part.absent {
			return nil, true, false, nil
		}
		if part.callErr != nil {
			for i := range part.errs {
				part.errs[i] = part.callErr
			}
		}
	}

	results = make([]bool, 0, len(events))
	errs = make([]errors.Error, 0, len(events))
	for _, part := range parts {
		results = append(results, part.result...)
		errs = append(errs, part.errs...)
	}
	return results, false, false, errs
}

// matchChunk is one production call: it matches a single shard on the serving pool (stable, or the
// canary slice - decided inside Call). A disabled matcher returns all-true.
func (p *Pool) matchChunk(ctx context.Context, matcherID string, eventChunk []evts.Event, canaryHashKey string) matchChunkResult {
	results := make([]bool, len(eventChunk))
	perErrs := make([]errors.Error, len(eventChunk))
	prodFn := func(callCtx context.Context, m Matcher) error {
		if !m.MatcherMetadata().Enabled {
			for i := range results {
				results[i] = true
			}
			return nil
		}
		batchResults, e := m.Match(callCtx, eventChunk)
		if e != nil {
			for i := range perErrs {
				perErrs[i] = e
			}
			return nil
		}
		if len(batchResults) != len(eventChunk) {
			e := errors.NewF("matcher %s returned %d results for %d events", matcherID, len(batchResults), len(eventChunk))
			for i := range perErrs {
				perErrs[i] = e
			}
			return nil
		}
		copy(results, batchResults)
		return nil
	}
	err := p.Call(ctx, matcherID, canaryHashKey, prodFn)
	if err != nil {
		if stderrors.Is(err, pools.ErrPluginNotFound) {
			return matchChunkResult{absent: true}
		}
		if stderrors.Is(err, pools.ErrPluginRemoved) {
			return matchChunkResult{removed: true}
		}
		return matchChunkResult{result: results, errs: perErrs, callErr: errors.NewE(err)}
	}
	return matchChunkResult{result: results, errs: perErrs}
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
			_, e := m.Match(callCtx, chunk)
			return e
		})
	}
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

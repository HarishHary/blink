package matchers

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/events"
)

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
	res []bool
	err errors.Error
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
func (p *Pool) Match(ctx context.Context, matcherID string, evts []events.Event, canaryHashKey string) ([]bool, errors.Error) {
	p.shadowMatch(ctx, matcherID, evts) // background mirror; no-op unless shadow mode

	// Production path: shard across the serving pool's workers, match each shard, concat in order.
	k := p.ServingPoolSize(matcherID, canaryHashKey)
	parts := pools.ShardConcurrent(evts, k, func(chunk []events.Event) matchChunkResult {
		r, e := p.matchChunk(ctx, matcherID, chunk, canaryHashKey)
		return matchChunkResult{r, e}
	})

	var results []bool
	for _, part := range parts {
		if part.err != nil { // first shard error wins
			return nil, part.err
		}
		results = append(results, part.res...)
	}
	return results, nil
}

// matchChunk is one production call: it matches a single shard on the serving pool (stable, or the
// canary slice - decided inside Call). A disabled matcher returns all-true.
func (p *Pool) matchChunk(ctx context.Context, matcherID string, evts []events.Event, canaryHashKey string) ([]bool, errors.Error) {
	var results []bool
	prodFn := func(callCtx context.Context, m Matcher) error {
		if !m.MatcherMetadata().Enabled {
			results = make([]bool, len(evts))
			for i := range results {
				results[i] = true
			}
			return nil
		}
		var e errors.Error
		results, e = m.Match(callCtx, evts)
		return e
	}
	err := p.Call(ctx, matcherID, canaryHashKey, prodFn)
	if err != nil {
		return nil, errors.NewE(err)
	}
	return results, nil
}

// shadowMatch fans the full batch out to the shadow candidate (if matcherID is in shadow mode) at its
// own max_procs, each shard a detached CallShadow whose result is dropped. Match is read-only on evts,
// so shards share the batch.
func (p *Pool) shadowMatch(ctx context.Context, matcherID string, evts []events.Event) {
	sk := p.ShadowPoolSize(matcherID)
	if sk == 0 || len(evts) == 0 {
		return
	}
	for _, chunk := range pools.ShardSlice(evts, sk) {
		chunk := chunk
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

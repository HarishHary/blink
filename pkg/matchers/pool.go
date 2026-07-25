package matchers

import (
	"context"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
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
func NewPool(logger *logger.Logger, cfg config.Source[*MatcherMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Matcher](logger, config.RolloutFor(cfg), pools.NewPoolMetrics("matchers"), drainTimeout),
	}
}

// MatchItem holds one event's match outcome.
type MatchItem struct {
	Matched bool
	Err     errors.Error
}

// MatchResult holds the result from matching events.
type MatchResult struct {
	Items   []MatchItem
	CallErr errors.Error // whole-call failure; never record-scoped
}

// Match runs matcher matcherID against every event and returns one item per event in input order
// (a disabled matcher passes through as all-true). Events are partitioned by their tenant rollout
// bucket before each group is sent through the process pool. It is deliberately mode-agnostic - the
// rollout mode is handled entirely inside the pool primitives. Two things happen:
//
//   - Production (all modes): each route group is sharded across the serving pool's workers and matched
//     concurrently. Call chooses the active or pending pool - Match does not need to know which.
//   - Shadow (shadow mode only): shadowMatch also mirrors the whole batch to the candidate in the
//     background at the candidate's own max_procs, dropping the result. It is a no-op in every other mode.
func (p *Pool) Match(ctx context.Context, matcherID string, events []evts.Event) MatchResult {
	p.shadowMatch(ctx, matcherID, events)

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		events     []evts.Event
	}
	groups := make([]routeGroup, 0)
	groupIndexByBucket := make(map[uint32]int)
	for i, event := range events {
		rolloutKey := pools.NormalizeRolloutKey(event["tenant_id"])
		bucket := pools.RolloutBucket(rolloutKey)
		groupIndex, ok := groupIndexByBucket[bucket]
		if !ok {
			groupIndex = len(groups)
			groupIndexByBucket[bucket] = groupIndex
			groups = append(groups, routeGroup{rolloutKey: rolloutKey})
		}
		groups[groupIndex].indexes = append(groups[groupIndex].indexes, i)
		groups[groupIndex].events = append(groups[groupIndex].events, event)
	}

	parts := make([]MatchResult, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := p.ServingPoolSize(matcherID, group.rolloutKey)
			shards := pools.ShardConcurrent(group.events, k, func(chunk []evts.Event) MatchResult {
				return p.matchChunk(ctx, matcherID, chunk, group.rolloutKey)
			})
			part := MatchResult{Items: make([]MatchItem, 0, len(group.events))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = MatchResult{CallErr: shard.CallErr}
					return
				}
				part.Items = append(part.Items, shard.Items...)
			}
			parts[i] = part
		}(i)
	}
	wg.Wait()

	result := MatchResult{Items: make([]MatchItem, len(events))}
	for i, part := range parts {
		if part.CallErr != nil {
			return MatchResult{CallErr: part.CallErr}
		}
		group := groups[i]
		if len(part.Items) != len(group.indexes) {
			return MatchResult{CallErr: errors.NewF("matcher %s returned invalid routed result shape", matcherID)}
		}
		for j, inputIndex := range group.indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

// matchChunk is one production call: it matches a single shard on the serving pool (stable, or the
// canary slice - decided inside Call). A disabled matcher returns all-true.
func (p *Pool) matchChunk(ctx context.Context, matcherID string, eventChunk []evts.Event, rolloutKey string) MatchResult {
	items := make([]MatchItem, len(eventChunk))
	prodFn := func(callCtx context.Context, m Matcher) error {
		if !m.MatcherMetadata().Enabled {
			for i := range items {
				items[i].Matched = true
			}
			return nil
		}
		batchResult := m.MatchBatch(callCtx, eventChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Items) != len(eventChunk) {
			return &errors.ResultCardinalityError{PluginKind: "matcher", PluginID: matcherID, Field: "items", Expected: len(eventChunk), Actual: len(batchResult.Items)}
		}
		copy(items, batchResult.Items)
		return nil
	}
	err := p.Call(ctx, matcherID, rolloutKey, prodFn)
	if err != nil {
		return MatchResult{CallErr: errors.NewE(err)}
	}
	return MatchResult{Items: items}
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

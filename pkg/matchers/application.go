package matchers

import (
	"context"
	"fmt"
	"sync"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/pkg/events"
)

// Application routes matcher calls through the Ergo plugin runtime.
type Application struct {
	*plugin.Application[Matcher, *MatcherMetadata]
}

// NewApplication creates a matcher plugin application.
func NewApplication(opts plugin.Options, runtimeLogger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, runtimeLogger)}
}

// Match runs one matcher against every event in a prepared batch and preserves input order.
func (r *Application) Match(ctx context.Context, state snapshot.ProjectionState[*MatcherMetadata], matcherID string, input *events.Batch) MatchResult {
	if r == nil || r.Application == nil {
		return MatchResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if input.Len() == 0 {
		return MatchResult{Items: []MatchItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[matcherID]
	callBudget := r.CallBudget(rollout)

	if rollout.Shadow {
		r.shadow(ctx, matcherID, input, callBudget, generation)
	}

	keys := input.RolloutKeys()
	groups := runtime.RouteSides(keys, rollout.CanaryPct)
	// One side takes the whole batch, so it goes as it stands: gathering it would copy the batch to
	// reproduce it and the items come back already in order.
	if groups == nil {
		return r.matchRoute(ctx, matcherID, keys[0], generation, input, callBudget)
	}

	parts := make([]MatchResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			parts[i] = r.matchRoute(ctx, matcherID, group.Key, generation, input.Gather(group.Indexes), workerBudget)
		})
	}
	wg.Wait()

	result := MatchResult{Items: make([]MatchItem, input.Len())}
	for i, part := range parts {
		if part.CallErr != nil {
			return MatchResult{CallErr: part.CallErr}
		}
		for j, inputIndex := range groups[i].Indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

// matchRoute runs one side's batch, cut by bytes across the calls it is allowed, and concatenates the
// items in batch order.
func (r *Application) matchRoute(ctx context.Context, matcherID, rolloutKey string, generation int64, batch *events.Batch, workers int) MatchResult {
	shards := runtime.ShardBytes(batch.WireSizes(), runtime.MaxCallPayloadBytes, workers, func(start, end int) MatchResult {
		return r.matchChunk(ctx, matcherID, rolloutKey, generation, batch.Slice(start, end))
	})
	result := MatchResult{Items: make([]MatchItem, 0, batch.Len())}
	for _, shard := range shards {
		if shard.CallErr != nil {
			return MatchResult{CallErr: shard.CallErr}
		}
		result.Items = append(result.Items, shard.Items...)
	}
	if len(result.Items) != batch.Len() {
		return MatchResult{CallErr: errors.NewF("matcher %s returned invalid routed result shape", matcherID)}
	}
	return result
}

func (r *Application) matchChunk(ctx context.Context, matcherID, rolloutKey string, generation int64, chunk *events.Batch) MatchResult {
	items := make([]MatchItem, chunk.Len())
	invocation, err := r.Application.Submit(ctx, matcherID, rolloutKey, generation, func(callCtx context.Context, matcher Matcher) error {
		if !matcher.MatcherMetadata().Enabled {
			for i := range items {
				items[i].Matched = true
			}
			return nil
		}
		result := matcher.MatchBatch(callCtx, chunk)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Items) != chunk.Len() {
			return &errors.ResultCardinalityError{PluginKind: "matcher", PluginID: matcherID, Field: "items", Expected: chunk.Len(), Actual: len(result.Items)}
		}
		copy(items, result.Items)
		return nil
	})
	if err != nil {
		return MatchResult{CallErr: errors.NewE(err)}
	}
	select {
	case <-invocation.Done():
		if err := invocation.Err(); err != nil {
			return MatchResult{CallErr: errors.NewE(err)}
		}
		return MatchResult{Items: items}
	case <-ctx.Done():
		invocation.Cancel(ctx.Err())
		return MatchResult{CallErr: errors.NewE(ctx.Err())}
	}
}

// shadow mirrors the batch onto a shadow candidate, cut the way production cuts it so an oversized
// payload fails the same way. SubmitShadow does not block; the runtime has its own shadow budget.
func (r *Application) shadow(ctx context.Context, matcherID string, input *events.Batch, callBudget int, generation int64) {
	bounds := runtime.ChunkBounds(input.WireSizes(), runtime.MaxCallPayloadBytes, callBudget)
	for i := range len(bounds) - 1 {
		chunk := input.Slice(bounds[i], bounds[i+1])
		_, _ = r.Application.SubmitShadow(ctx, matcherID, generation, func(callCtx context.Context, matcher Matcher) error {
			if !matcher.MatcherMetadata().Enabled {
				return nil
			}
			result := matcher.MatchBatch(callCtx, chunk)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != chunk.Len() {
				return fmt.Errorf("matcher %s returned %d items for %d shadow events", matcherID, len(result.Items), chunk.Len())
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

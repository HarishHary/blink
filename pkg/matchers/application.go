package matchers

import (
	"context"
	"fmt"
	"sync"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	snapshotruntime "github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/pkg/events"
)

// Application preserves matcher batching semantics while routing calls through
// the Ergo plugin runtime.
type Application struct {
	*plugin.Application[Matcher, *MatcherMetadata]
}

// NewApplication creates a matcher plugin application.
func NewApplication(opts plugin.Options, runtimeLogger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, runtimeLogger)}
}

// Match runs one matcher against every event and preserves input order.
func (r *Application) Match(ctx context.Context, state snapshotruntime.ProjectionState[*MatcherMetadata], matcherID string, input []events.Event) MatchResult {
	if r == nil || r.Application == nil {
		return MatchResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if len(input) == 0 {
		return MatchResult{Items: []MatchItem{}}
	}
	generation := state.CommittedGeneration
	r.shadow(ctx, matcherID, input, max(1, state.MaxProcsByID[matcherID]), generation)

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		events     []events.Event
	}
	groups := make([]routeGroup, 0)
	groupByBucket := make(map[uint32]int)
	for i, event := range input {
		rolloutKey := runtime.NormalizeRolloutKey(event["tenant_id"])
		bucket := runtime.RolloutBucket(rolloutKey)
		groupIndex, ok := groupByBucket[bucket]
		if !ok {
			groupIndex = len(groups)
			groupByBucket[bucket] = groupIndex
			groups = append(groups, routeGroup{rolloutKey: rolloutKey})
		}
		groups[groupIndex].indexes = append(groups[groupIndex].indexes, i)
		groups[groupIndex].events = append(groups[groupIndex].events, event)
	}

	parts := make([]MatchResult, len(groups))
	workerCount := max(1, state.MaxProcsByID[matcherID])
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shards := runtime.ShardConcurrent(group.events, workerCount, func(chunk []events.Event) MatchResult {
				return r.matchChunk(ctx, matcherID, group.rolloutKey, generation, chunk)
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
		}()
	}
	wg.Wait()

	result := MatchResult{Items: make([]MatchItem, len(input))}
	for i, part := range parts {
		if part.CallErr != nil {
			return MatchResult{CallErr: part.CallErr}
		}
		if len(part.Items) != len(groups[i].indexes) {
			return MatchResult{CallErr: errors.NewF("matcher %s returned invalid routed result shape", matcherID)}
		}
		for j, inputIndex := range groups[i].indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

func (r *Application) matchChunk(ctx context.Context, matcherID, rolloutKey string, generation int64, input []events.Event) MatchResult {
	owned := events.CloneEvents(input)
	items := make([]MatchItem, len(owned))
	invocation, err := r.Application.Submit(ctx, matcherID, rolloutKey, generation, func(callCtx context.Context, matcher Matcher) error {
		if !matcher.MatcherMetadata().Enabled {
			for i := range items {
				items[i].Matched = true
			}
			return nil
		}
		result := matcher.MatchBatch(callCtx, owned)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Items) != len(owned) {
			return &errors.ResultCardinalityError{PluginKind: "matcher", PluginID: matcherID, Field: "items", Expected: len(owned), Actual: len(result.Items)}
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

func (r *Application) shadow(ctx context.Context, matcherID string, input []events.Event, workerCount int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, workerCount) {
		owned := events.CloneEvents(chunk)
		_, _ = r.Application.SubmitShadow(ctx, matcherID, generation, func(callCtx context.Context, matcher Matcher) error {
			if !matcher.MatcherMetadata().Enabled {
				return nil
			}
			result := matcher.MatchBatch(callCtx, owned)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != len(owned) {
				return fmt.Errorf("matcher %s returned %d items for %d shadow events", matcherID, len(result.Items), len(owned))
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

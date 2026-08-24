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

// Application routes matcher calls through the Ergo plugin runtime.
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
	rollout := state.RolloutByID[matcherID]
	callBudget := r.CallBudget(rollout)
	eventBytes := events.SampleWireSize(input)

	if rollout.Shadow {
		r.shadow(ctx, matcherID, input, callBudget, eventBytes, generation)
	}

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		events     []events.Event
	}

	groups := make([]routeGroup, 0, 2)
	groupBySide := [2]int{-1, -1}
	for i, event := range input {
		rolloutKey := runtime.NormalizeRolloutKey(event["tenant_id"])
		side := 0
		if rollout.CanarySide(rolloutKey) {
			side = 1
		}
		if groupBySide[side] < 0 {
			groupBySide[side] = len(groups)
			groups = append(groups, routeGroup{rolloutKey: rolloutKey})
		}
		group := &groups[groupBySide[side]]
		group.indexes = append(group.indexes, i)
		group.events = append(group.events, event)
	}

	parts := make([]MatchResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			chunks := runtime.MaxChunks(len(group.events), workerBudget, eventBytes)
			shards := runtime.ShardPooled(group.events, chunks, workerBudget, func(chunk []events.Event) MatchResult {
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
		})
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

// shadow mirrors the batch onto a shadow candidate, cut the way production cuts it so an oversized
// payload fails the same way. SubmitShadow does not block; the runtime has its own shadow budget.
func (r *Application) shadow(ctx context.Context, matcherID string, input []events.Event, callBudget, eventBytes int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, runtime.MaxChunks(len(input), callBudget, eventBytes)) {
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

package rules

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

// Application routes rule evaluation through the Ergo plugin runtime.
type Application struct {
	*plugin.Application[Rule, *RuleMetadata]
}

// NewApplication creates a rule plugin application.
func NewApplication(opts plugin.Options, logger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, logger)}
}

// Evaluate preserves input order while grouping events by rollout side and sharding each group.
func (r *Application) Evaluate(ctx context.Context, state snapshotruntime.ProjectionState[*RuleMetadata], ruleID string, input *events.Batch) EvaluateResult {
	if r == nil || r.Application == nil {
		return EvaluateResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if input.Len() == 0 {
		return EvaluateResult{Items: []EvaluateItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[ruleID]
	callBudget := r.CallBudget(rollout)

	if rollout.Shadow {
		r.shadow(ctx, ruleID, input, callBudget, generation)
	}

	keys := input.RolloutKeys()
	groups := runtime.RouteSides(keys, rollout.CanaryPct)
	// One side takes the whole batch, so it goes as it stands: gathering it would copy the batch to
	// reproduce it and the items come back already in order.
	if groups == nil {
		return r.evaluateRoute(ctx, ruleID, keys[0], generation, input, callBudget)
	}

	parts := make([]EvaluateResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			parts[i] = r.evaluateRoute(ctx, ruleID, group.Key, generation, input.Gather(group.Indexes), workerBudget)
		})
	}
	wg.Wait()

	result := EvaluateResult{Items: make([]EvaluateItem, input.Len())}
	for i, part := range parts {
		if part.CallErr != nil {
			return EvaluateResult{CallErr: part.CallErr}
		}
		for j, inputIndex := range groups[i].Indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

// evaluateRoute runs one side's batch, cut by bytes across the calls it is allowed, and concatenates
// the items in batch order.
func (r *Application) evaluateRoute(ctx context.Context, ruleID, rolloutKey string, generation int64, batch *events.Batch, workers int) EvaluateResult {
	shards := runtime.ShardBytes(batch.WireSizes(), runtime.MaxCallPayloadBytes, workers, func(start, end int) EvaluateResult {
		return r.evaluateChunk(ctx, ruleID, rolloutKey, generation, batch.Slice(start, end))
	})
	result := EvaluateResult{Items: make([]EvaluateItem, 0, batch.Len())}
	for _, shard := range shards {
		if shard.CallErr != nil {
			return EvaluateResult{CallErr: shard.CallErr}
		}
		result.Items = append(result.Items, shard.Items...)
	}
	if len(result.Items) != batch.Len() {
		return EvaluateResult{CallErr: errors.NewF("rule %s returned invalid routed result shape", ruleID)}
	}
	return result
}

func (r *Application) evaluateChunk(ctx context.Context, ruleID, rolloutKey string, generation int64, chunk *events.Batch) EvaluateResult {
	items := make([]EvaluateItem, chunk.Len())
	invocation, err := r.Application.Submit(ctx, ruleID, rolloutKey, generation, func(callCtx context.Context, rule Rule) error {
		if !rule.RuleMetadata().Enabled {
			return nil
		}
		result := rule.EvaluateBatch(callCtx, chunk)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Items) != chunk.Len() {
			return &errors.ResultCardinalityError{PluginKind: "rule", PluginID: ruleID, Field: "items", Expected: chunk.Len(), Actual: len(result.Items)}
		}
		copy(items, result.Items)
		return nil
	})
	if err != nil {
		return EvaluateResult{CallErr: errors.NewE(err)}
	}
	select {
	case <-invocation.Done():
		if err := invocation.Err(); err != nil {
			return EvaluateResult{CallErr: errors.NewE(err)}
		}
		return EvaluateResult{Items: items}
	case <-ctx.Done():
		invocation.Cancel(ctx.Err())
		return EvaluateResult{CallErr: errors.NewE(ctx.Err())}
	}
}

// shadow mirrors the batch onto a shadow candidate, cut the way production cuts it so an oversized
// payload fails the same way. SubmitShadow does not block; the runtime has its own shadow budget.
func (r *Application) shadow(ctx context.Context, ruleID string, input *events.Batch, callBudget int, generation int64) {
	bounds := runtime.ChunkBounds(input.WireSizes(), runtime.MaxCallPayloadBytes, callBudget)
	for i := range len(bounds) - 1 {
		chunk := input.Slice(bounds[i], bounds[i+1])
		_, _ = r.Application.SubmitShadow(ctx, ruleID, generation, func(callCtx context.Context, rule Rule) error {
			if !rule.RuleMetadata().Enabled {
				return nil
			}
			result := rule.EvaluateBatch(callCtx, chunk)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != chunk.Len() {
				return fmt.Errorf("rule %s returned %d items for %d shadow events", ruleID, len(result.Items), chunk.Len())
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

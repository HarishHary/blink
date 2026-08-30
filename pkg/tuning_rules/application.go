package tuning_rules

import (
	"context"
	"fmt"
	"sync"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/pkg/alerts"
)

// Application routes tuning-rule calls through the Ergo plugin runtime.
type Application struct {
	*plugin.Application[TuningRule, *TuningRuleMetadata]
}

// NewApplication creates a tuning-rule plugin application.
func NewApplication(opts plugin.ApplicationOptions, logger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, logger)}
}

// Tune runs one tuning rule across every alert and preserves input order.
func (r *Application) Tune(ctx context.Context, state snapshot.ProjectionState[*TuningRuleMetadata], tuningRuleID string, input *alerts.Batch) TuneResult {
	if r == nil || r.Application == nil {
		return TuneResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if input.Len() == 0 {
		return TuneResult{Items: []TuneItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[tuningRuleID]
	callBudget := r.CallBudget(rollout)

	if rollout.Shadow {
		r.shadow(ctx, tuningRuleID, input, callBudget, generation)
	}

	keys := input.RolloutKeys()
	groups := runtime.RouteSides(keys, rollout.CanaryPct)
	// One side takes the whole batch, so it goes as it stands: gathering it would copy the batch to
	// reproduce it and the items come back already in order.
	if groups == nil {
		return r.tuneRoute(ctx, tuningRuleID, keys[0], generation, input, callBudget)
	}

	parts := make([]TuneResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			parts[i] = r.tuneRoute(ctx, tuningRuleID, group.Key, generation, input.Gather(group.Indexes), workerBudget)
		})
	}
	wg.Wait()

	result := TuneResult{Items: make([]TuneItem, input.Len())}
	for i, part := range parts {
		if part.CallErr != nil {
			return TuneResult{CallErr: part.CallErr}
		}
		for j, inputIndex := range groups[i].Indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

// tuneRoute runs one side's alerts, cut by bytes across the calls it is allowed, and concatenates the
// items in input order.
func (r *Application) tuneRoute(ctx context.Context, tuningRuleID, rolloutKey string, generation int64, input *alerts.Batch, workers int) TuneResult {
	shards := runtime.ShardBytes(input.WireSizes(), runtime.MaxCallPayloadBytes, workers, func(start, end int) TuneResult {
		return r.tuneChunk(ctx, tuningRuleID, rolloutKey, generation, input.Slice(start, end))
	})
	result := TuneResult{Items: make([]TuneItem, 0, input.Len())}
	for _, shard := range shards {
		if shard.CallErr != nil {
			return TuneResult{CallErr: shard.CallErr}
		}
		result.Items = append(result.Items, shard.Items...)
	}
	if len(result.Items) != input.Len() {
		return TuneResult{CallErr: errors.NewF("tuning rule %s returned invalid routed result shape", tuningRuleID)}
	}
	return result
}

func (r *Application) tuneChunk(ctx context.Context, tuningRuleID, rolloutKey string, generation int64, input *alerts.Batch) TuneResult {
	items := make([]TuneItem, input.Len())
	invocation, err := r.Application.Submit(ctx, tuningRuleID, rolloutKey, generation, func(callCtx context.Context, rule TuningRule) error {
		metadata := rule.TuningRuleMetadata()
		if !metadata.Enabled {
			return nil
		}
		for i := range items {
			items[i].RuleType = metadata.RuleType
			items[i].Confidence = metadata.Confidence
		}
		result := rule.TuneBatch(callCtx, input)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Items) != input.Len() {
			return &errors.ResultCardinalityError{PluginKind: "tuning rule", PluginID: tuningRuleID, Field: "items", Expected: input.Len(), Actual: len(result.Items)}
		}
		for i, item := range result.Items {
			items[i].Applies = item.Applies
			items[i].Err = item.Err
		}
		return nil
	})
	if err != nil {
		return TuneResult{CallErr: errors.NewE(err)}
	}
	select {
	case <-invocation.Done():
		if err := invocation.Err(); err != nil {
			return TuneResult{CallErr: errors.NewE(err)}
		}
		return TuneResult{Items: items}
	case <-ctx.Done():
		invocation.Cancel(ctx.Err())
		return TuneResult{CallErr: errors.NewE(ctx.Err())}
	}
}

// shadow mirrors the batch onto a shadow candidate, cut the way production cuts it so an oversized
// payload fails the same way. SubmitShadow does not block; the runtime has its own shadow budget.
func (r *Application) shadow(ctx context.Context, tuningRuleID string, input *alerts.Batch, callBudget int, generation int64) {
	bounds := runtime.ChunkBounds(input.WireSizes(), runtime.MaxCallPayloadBytes, callBudget)
	for i := range len(bounds) - 1 {
		chunk := input.Slice(bounds[i], bounds[i+1])
		_, _ = r.Application.SubmitShadow(ctx, tuningRuleID, generation, func(callCtx context.Context, rule TuningRule) error {
			if !rule.TuningRuleMetadata().Enabled {
				return nil
			}
			result := rule.TuneBatch(callCtx, chunk)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != chunk.Len() {
				return fmt.Errorf("tuning rule %s returned %d items for %d shadow alerts", tuningRuleID, len(result.Items), chunk.Len())
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

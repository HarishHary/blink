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
func NewApplication(opts plugin.Options, runtimeLogger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, runtimeLogger)}
}

// Evaluate preserves input order while grouping tenant rollout buckets and sharding each route.
func (r *Application) Evaluate(ctx context.Context, state snapshotruntime.ProjectionState[*RuleMetadata], ruleID string, input []events.Event) EvaluateResult {
	if r == nil || r.Application == nil {
		return EvaluateResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if len(input) == 0 {
		return EvaluateResult{Items: []EvaluateItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[ruleID]
	workerCount := max(1, rollout.MaxProcs)
	// Without a shadow candidate in this generation every submission clones the batch only
	// to be rejected as unroutable, one logged error per shard.
	if rollout.Shadow {
		r.shadow(ctx, ruleID, input, workerCount, generation)
	}

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

	parts := make([]EvaluateResult, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shards := runtime.ShardConcurrent(group.events, workerCount, func(chunk []events.Event) EvaluateResult {
				return r.evaluateChunk(ctx, ruleID, group.rolloutKey, generation, chunk)
			})
			part := EvaluateResult{Items: make([]EvaluateItem, 0, len(group.events))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = EvaluateResult{CallErr: shard.CallErr}
					return
				}
				part.Items = append(part.Items, shard.Items...)
			}
			parts[i] = part
		}()
	}
	wg.Wait()

	result := EvaluateResult{Items: make([]EvaluateItem, len(input))}
	for i, part := range parts {
		if part.CallErr != nil {
			return EvaluateResult{CallErr: part.CallErr}
		}
		if len(part.Items) != len(groups[i].indexes) {
			return EvaluateResult{CallErr: errors.NewF("rule %s returned invalid routed result shape", ruleID)}
		}
		for j, inputIndex := range groups[i].indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

func (r *Application) evaluateChunk(ctx context.Context, ruleID, rolloutKey string, generation int64, input []events.Event) EvaluateResult {
	owned := events.CloneEvents(input)
	items := make([]EvaluateItem, len(owned))
	invocation, err := r.Application.Submit(ctx, ruleID, rolloutKey, generation, func(callCtx context.Context, rule Rule) error {
		if !rule.RuleMetadata().Enabled {
			return nil
		}
		result := rule.EvaluateBatch(callCtx, owned)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Items) != len(owned) {
			return &errors.ResultCardinalityError{PluginKind: "rule", PluginID: ruleID, Field: "items", Expected: len(owned), Actual: len(result.Items)}
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

func (r *Application) shadow(ctx context.Context, ruleID string, input []events.Event, workerCount int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, workerCount) {
		owned := events.CloneEvents(chunk)
		_, _ = r.Application.SubmitShadow(ctx, ruleID, generation, func(callCtx context.Context, rule Rule) error {
			if !rule.RuleMetadata().Enabled {
				return nil
			}
			result := rule.EvaluateBatch(callCtx, owned)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != len(owned) {
				return fmt.Errorf("rule %s returned %d items for %d shadow events", ruleID, len(result.Items), len(owned))
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

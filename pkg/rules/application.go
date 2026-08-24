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

// Evaluate preserves input order while grouping events by rollout side and sharding each group.
func (r *Application) Evaluate(ctx context.Context, state snapshotruntime.ProjectionState[*RuleMetadata], ruleID string, input []events.Event) EvaluateResult {
	if r == nil || r.Application == nil {
		return EvaluateResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if len(input) == 0 {
		return EvaluateResult{Items: []EvaluateItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[ruleID]
	callBudget := r.CallBudget(rollout)
	eventBytes := events.SampleWireSize(input)

	if rollout.Shadow {
		r.shadow(ctx, ruleID, input, callBudget, eventBytes, generation)
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

	parts := make([]EvaluateResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			chunks := runtime.MaxChunks(len(group.events), workerBudget, eventBytes)
			shards := runtime.ShardPooled(group.events, chunks, workerBudget, func(chunk []events.Event) EvaluateResult {
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
		})
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

// shadow mirrors the batch onto a shadow candidate, cut the way production cuts it so an oversized
// payload fails the same way. SubmitShadow does not block; the runtime has its own shadow budget.
func (r *Application) shadow(ctx context.Context, ruleID string, input []events.Event, callBudget, eventBytes int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, runtime.MaxChunks(len(input), callBudget, eventBytes)) {
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

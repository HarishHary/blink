package tuning_rules

import (
	"context"
	"fmt"
	"sync"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	snapshotruntime "github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/pkg/alerts"
)

// Application routes tuning-rule calls through the Ergo plugin runtime.
type Application struct {
	*plugin.Application[TuningRule, *TuningRuleMetadata]
}

// NewApplication creates a tuning-rule plugin application.
func NewApplication(opts plugin.Options, runtimeLogger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, runtimeLogger)}
}

// Tune runs one tuning rule across every alert and preserves input order.
func (r *Application) Tune(ctx context.Context, state snapshotruntime.ProjectionState[*TuningRuleMetadata], tuningRuleID string, input []*alerts.Alert) TuneResult {
	if r == nil || r.Application == nil {
		return TuneResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if len(input) == 0 {
		return TuneResult{Items: []TuneItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[tuningRuleID]
	callBudget := r.CallBudget(rollout)
	alertBytes := alerts.SampleWireSize(input)

	if rollout.Shadow {
		r.shadow(ctx, tuningRuleID, input, callBudget, alertBytes, generation)
	}

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		alerts     []*alerts.Alert
	}

	groups := make([]routeGroup, 0, 2)
	groupBySide := [2]int{-1, -1}
	for i, alert := range input {
		rolloutKey := runtime.NormalizeRolloutKey(alert.Event["tenant_id"])
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
		group.alerts = append(group.alerts, alert)
	}

	parts := make([]TuneResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			chunks := runtime.MaxChunks(len(group.alerts), workerBudget, alertBytes)
			shards := runtime.ShardPooled(group.alerts, chunks, workerBudget, func(chunk []*alerts.Alert) TuneResult {
				return r.tuneChunk(ctx, tuningRuleID, group.rolloutKey, generation, chunk)
			})
			part := TuneResult{Items: make([]TuneItem, 0, len(group.alerts))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = TuneResult{CallErr: shard.CallErr}
					return
				}
				part.Items = append(part.Items, shard.Items...)
			}
			parts[i] = part
		})
	}
	wg.Wait()

	result := TuneResult{Items: make([]TuneItem, len(input))}
	for i, part := range parts {
		if part.CallErr != nil {
			return TuneResult{CallErr: part.CallErr}
		}
		if len(part.Items) != len(groups[i].indexes) {
			return TuneResult{CallErr: errors.NewF("tuning rule %s returned invalid routed result shape", tuningRuleID)}
		}
		for j, inputIndex := range groups[i].indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

func (r *Application) tuneChunk(ctx context.Context, tuningRuleID, rolloutKey string, generation int64, input []*alerts.Alert) TuneResult {
	items := make([]TuneItem, len(input))
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
		if len(result.Items) != len(input) {
			return &errors.ResultCardinalityError{PluginKind: "tuning rule", PluginID: tuningRuleID, Field: "items", Expected: len(input), Actual: len(result.Items)}
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
func (r *Application) shadow(ctx context.Context, tuningRuleID string, input []*alerts.Alert, callBudget, alertBytes int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, runtime.MaxChunks(len(input), callBudget, alertBytes)) {
		owned := alerts.CloneAlerts(chunk)
		_, _ = r.Application.SubmitShadow(ctx, tuningRuleID, generation, func(callCtx context.Context, rule TuningRule) error {
			if !rule.TuningRuleMetadata().Enabled {
				return nil
			}
			result := rule.TuneBatch(callCtx, owned)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != len(owned) {
				return fmt.Errorf("tuning rule %s returned %d items for %d shadow alerts", tuningRuleID, len(result.Items), len(owned))
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

package formatters

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

// Application routes formatter calls through the Ergo plugin runtime.
type Application struct {
	*plugin.Application[Formatter, *FormatterMetadata]
}

// NewApplication creates a formatter plugin application.
func NewApplication(opts plugin.Options, runtimeLogger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, runtimeLogger)}
}

// Format preserves input order while grouping alerts by rollout side and sharding each group.
func (r *Application) Format(ctx context.Context, state snapshot.ProjectionState[*FormatterMetadata], formatterID string, input []*alerts.Alert) FormatResult {
	if r == nil || r.Application == nil {
		return FormatResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if len(input) == 0 {
		return FormatResult{Items: []FormatItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[formatterID]
	callBudget := r.CallBudget(rollout)
	alertBytes := alerts.SampleWireSize(input)

	if rollout.Shadow {
		r.shadow(ctx, formatterID, input, callBudget, alertBytes, generation)
	}

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		alerts     []*alerts.Alert
	}

	groups := make([]routeGroup, 0, 2)
	groupBySide := [2]int{-1, -1}
	for i, alert := range input {
		rolloutKey := runtime.MissingTenantRolloutKey
		if alert != nil {
			rolloutKey = runtime.NormalizeRolloutKey(alert.Event["tenant_id"])
		}
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

	parts := make([]FormatResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			chunks := runtime.MaxChunks(len(group.alerts), workerBudget, alertBytes)
			shards := runtime.ShardPooled(group.alerts, chunks, workerBudget, func(chunk []*alerts.Alert) FormatResult {
				return r.formatChunk(ctx, formatterID, group.rolloutKey, generation, chunk)
			})
			part := FormatResult{Items: make([]FormatItem, 0, len(group.alerts))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = FormatResult{CallErr: shard.CallErr}
					return
				}
				part.Items = append(part.Items, shard.Items...)
			}
			parts[i] = part
		})
	}
	wg.Wait()

	result := FormatResult{Items: make([]FormatItem, len(input))}
	for i, part := range parts {
		if part.CallErr != nil {
			return FormatResult{CallErr: part.CallErr}
		}
		if len(part.Items) != len(groups[i].indexes) {
			return FormatResult{CallErr: errors.NewF("formatter %s returned invalid routed result shape", formatterID)}
		}
		for j, inputIndex := range groups[i].indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

func (r *Application) formatChunk(ctx context.Context, formatterID, rolloutKey string, generation int64, input []*alerts.Alert) FormatResult {
	owned := alerts.CloneAlerts(input)
	items := make([]FormatItem, len(owned))
	invocation, err := r.Application.Submit(ctx, formatterID, rolloutKey, generation, func(callCtx context.Context, formatter Formatter) error {
		if !formatter.FormatterMetadata().Enabled {
			return nil
		}
		result := formatter.FormatBatch(callCtx, owned)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Items) != len(owned) {
			return &errors.ResultCardinalityError{PluginKind: "formatter", PluginID: formatterID, Field: "items", Expected: len(owned), Actual: len(result.Items)}
		}
		copy(items, result.Items)
		return nil
	})
	if err != nil {
		return FormatResult{CallErr: errors.NewE(err)}
	}
	select {
	case <-invocation.Done():
		if err := invocation.Err(); err != nil {
			return FormatResult{CallErr: errors.NewE(err)}
		}
		return FormatResult{Items: items}
	case <-ctx.Done():
		invocation.Cancel(ctx.Err())
		return FormatResult{CallErr: errors.NewE(ctx.Err())}
	}
}

// shadow mirrors the batch onto a shadow candidate, cut the way production cuts it so an oversized
// payload fails the same way. SubmitShadow does not block; the runtime has its own shadow budget.
func (r *Application) shadow(ctx context.Context, formatterID string, input []*alerts.Alert, callBudget, alertBytes int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, runtime.MaxChunks(len(input), callBudget, alertBytes)) {
		owned := alerts.CloneAlerts(chunk)
		_, _ = r.Application.SubmitShadow(ctx, formatterID, generation, func(callCtx context.Context, formatter Formatter) error {
			if !formatter.FormatterMetadata().Enabled {
				return nil
			}
			result := formatter.FormatBatch(callCtx, owned)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != len(owned) {
				return fmt.Errorf("formatter %s returned %d items for %d shadow alerts", formatterID, len(result.Items), len(owned))
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

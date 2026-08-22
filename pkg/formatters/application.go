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

// Format preserves input order while grouping rollout buckets and sharding each
// group into contiguous batches.
func (r *Application) Format(ctx context.Context, state snapshot.ProjectionState[*FormatterMetadata], formatterID string, input []*alerts.Alert) FormatResult {
	if r == nil || r.Application == nil {
		return FormatResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if len(input) == 0 {
		return FormatResult{Items: []FormatItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[formatterID]
	workerCount := max(1, rollout.MaxProcs)
	// Without a shadow candidate in this generation every submission clones the batch only
	// to be rejected as unroutable, one logged error per shard.
	if rollout.Shadow {
		r.shadow(ctx, formatterID, input, workerCount, generation)
	}

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		alerts     []*alerts.Alert
	}
	groups := make([]routeGroup, 0)
	groupByBucket := make(map[uint32]int)
	for i, alert := range input {
		rolloutKey := runtime.MissingTenantRolloutKey
		if alert != nil {
			rolloutKey = runtime.NormalizeRolloutKey(alert.Event["tenant_id"])
		}
		bucket := runtime.RolloutBucket(rolloutKey)
		groupIndex, ok := groupByBucket[bucket]
		if !ok {
			groupIndex = len(groups)
			groupByBucket[bucket] = groupIndex
			groups = append(groups, routeGroup{rolloutKey: rolloutKey})
		}
		groups[groupIndex].indexes = append(groups[groupIndex].indexes, i)
		groups[groupIndex].alerts = append(groups[groupIndex].alerts, alert)
	}

	parts := make([]FormatResult, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shards := runtime.ShardConcurrent(group.alerts, workerCount, func(chunk []*alerts.Alert) FormatResult {
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
		}()
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

func (r *Application) shadow(ctx context.Context, formatterID string, input []*alerts.Alert, workerCount int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, workerCount) {
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

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
func NewApplication(opts plugin.ApplicationOptions, logger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, logger)}
}

// Format preserves input order while grouping alerts by rollout side and sharding each group.
func (r *Application) Format(ctx context.Context, state snapshot.ProjectionState[*FormatterMetadata], formatterID string, input *alerts.Batch) FormatResult {
	if r == nil || r.Application == nil {
		return FormatResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if input.Len() == 0 {
		return FormatResult{Items: []FormatItem{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[formatterID]
	callBudget := r.CallBudget(rollout)

	if rollout.Shadow {
		r.shadow(ctx, formatterID, input, callBudget, generation)
	}

	keys := input.RolloutKeys()
	groups := runtime.RouteSides(keys, rollout.CanaryPct)
	// One side takes the whole batch, so it goes as it stands: gathering it would copy the batch to
	// reproduce it and the items come back already in order.
	if groups == nil {
		return r.formatRoute(ctx, formatterID, keys[0], generation, input, callBudget)
	}

	parts := make([]FormatResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			parts[i] = r.formatRoute(ctx, formatterID, group.Key, generation, input.Gather(group.Indexes), workerBudget)
		})
	}
	wg.Wait()

	result := FormatResult{Items: make([]FormatItem, input.Len())}
	for i, part := range parts {
		if part.CallErr != nil {
			return FormatResult{CallErr: part.CallErr}
		}
		for j, inputIndex := range groups[i].Indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

// formatRoute runs one side's alerts, cut by bytes across the calls it is allowed, and concatenates
// the items in input order.
func (r *Application) formatRoute(ctx context.Context, formatterID, rolloutKey string, generation int64, input *alerts.Batch, workers int) FormatResult {
	shards := runtime.ShardBytes(input.WireSizes(), runtime.MaxCallPayloadBytes, workers, func(start, end int) FormatResult {
		return r.formatChunk(ctx, formatterID, rolloutKey, generation, input.Slice(start, end))
	})
	result := FormatResult{Items: make([]FormatItem, 0, input.Len())}
	for _, shard := range shards {
		if shard.CallErr != nil {
			return FormatResult{CallErr: shard.CallErr}
		}
		result.Items = append(result.Items, shard.Items...)
	}
	if len(result.Items) != input.Len() {
		return FormatResult{CallErr: errors.NewF("formatter %s returned invalid routed result shape", formatterID)}
	}
	return result
}

func (r *Application) formatChunk(ctx context.Context, formatterID, rolloutKey string, generation int64, input *alerts.Batch) FormatResult {
	items := make([]FormatItem, input.Len())
	invocation, err := r.Application.Submit(ctx, formatterID, rolloutKey, generation, func(callCtx context.Context, formatter Formatter) error {
		if !formatter.FormatterMetadata().Enabled {
			return nil
		}
		result := formatter.FormatBatch(callCtx, input)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Items) != input.Len() {
			return &errors.ResultCardinalityError{PluginKind: "formatter", PluginID: formatterID, Field: "items", Expected: input.Len(), Actual: len(result.Items)}
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
func (r *Application) shadow(ctx context.Context, formatterID string, input *alerts.Batch, callBudget int, generation int64) {
	bounds := runtime.ChunkBounds(input.WireSizes(), runtime.MaxCallPayloadBytes, callBudget)
	for i := range len(bounds) - 1 {
		chunk := input.Slice(bounds[i], bounds[i+1])
		_, _ = r.Application.SubmitShadow(ctx, formatterID, generation, func(callCtx context.Context, formatter Formatter) error {
			if !formatter.FormatterMetadata().Enabled {
				return nil
			}
			result := formatter.FormatBatch(callCtx, chunk)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Items) != chunk.Len() {
				return fmt.Errorf("formatter %s returned %d items for %d shadow alerts", formatterID, len(result.Items), chunk.Len())
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

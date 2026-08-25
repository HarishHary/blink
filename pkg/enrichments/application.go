package enrichments

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

// Application preserves enrichment routing while dispatching through Ergo.
type Application struct {
	*plugin.Application[Enrichment, *EnrichmentMetadata]
}

// NewApplication creates an enrichment plugin application.
func NewApplication(opts plugin.Options, runtimeLogger *logger.Logger) *Application {
	return &Application{Application: plugin.NewApplication(opts, NewAdapter(), Loader{}, runtimeLogger)}
}

// Enrich applies one enrichment to every alert, preserving input order and cardinality.
func (r *Application) Enrich(ctx context.Context, state snapshot.ProjectionState[*EnrichmentMetadata], enrichmentID string, input *alerts.Batch) EnrichResult {
	if r == nil || r.Application == nil {
		return EnrichResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if input.Len() == 0 {
		return EnrichResult{Errs: []errors.Error{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[enrichmentID]
	callBudget := r.CallBudget(rollout)

	if rollout.Shadow {
		r.shadow(ctx, enrichmentID, input, callBudget, generation)
	}
	// The plugin's enrichment lands on the alerts the call carried, so it lands on copies until the
	// error beside each one says to keep it. Copying the batch does not re-encode it.
	owned := input.Clone()

	keys := input.RolloutKeys()
	groups := runtime.RouteSides(keys, rollout.CanaryPct)
	// One side takes the whole batch, so it goes as it stands: gathering it would copy the batch to
	// reproduce it and the errors come back already in order.
	if groups == nil {
		result := r.enrichRoute(ctx, enrichmentID, keys[0], generation, owned, callBudget)
		if result.CallErr != nil {
			return result
		}
		for i, err := range result.Errs {
			if err == nil {
				*input.At(i) = *owned.At(i)
			}
		}
		return result
	}

	parts := make([]EnrichResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			parts[i] = r.enrichRoute(ctx, enrichmentID, group.Key, generation, owned.Gather(group.Indexes), workerBudget)
		})
	}
	wg.Wait()

	result := EnrichResult{Errs: make([]errors.Error, input.Len())}
	for i, part := range parts {
		if part.CallErr != nil {
			return EnrichResult{CallErr: part.CallErr}
		}
		for j, inputIndex := range groups[i].Indexes {
			result.Errs[inputIndex] = part.Errs[j]
			if part.Errs[j] == nil {
				*input.At(inputIndex) = *owned.At(inputIndex)
			}
		}
	}
	return result
}

// enrichRoute runs one side's alerts, cut by bytes across the calls it is allowed, and concatenates
// the errors in input order. The caller commits the alerts whose error is nil; the rest are not its
// copies to keep.
func (r *Application) enrichRoute(ctx context.Context, enrichmentID, rolloutKey string, generation int64, input *alerts.Batch, workers int) EnrichResult {
	shards := runtime.ShardBytes(input.WireSizes(), runtime.MaxCallPayloadBytes, workers, func(start, end int) EnrichResult {
		return r.enrichChunk(ctx, enrichmentID, rolloutKey, generation, input.Slice(start, end))
	})
	result := EnrichResult{Errs: make([]errors.Error, 0, input.Len())}
	for _, shard := range shards {
		if shard.CallErr != nil {
			return EnrichResult{CallErr: shard.CallErr}
		}
		result.Errs = append(result.Errs, shard.Errs...)
	}
	if len(result.Errs) != input.Len() {
		return EnrichResult{CallErr: errors.NewF("enrichment %s returned invalid routed result shape", enrichmentID)}
	}
	return result
}

func (r *Application) enrichChunk(ctx context.Context, enrichmentID, rolloutKey string, generation int64, input *alerts.Batch) EnrichResult {
	errs := make([]errors.Error, input.Len())
	invocation, err := r.Application.Submit(ctx, enrichmentID, rolloutKey, generation, func(callCtx context.Context, enrichment Enrichment) error {
		if !enrichment.EnrichmentMetadata().Enabled {
			return nil
		}
		result := enrichment.EnrichBatch(callCtx, input)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Errs) != input.Len() {
			return &errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: enrichmentID, Field: "errors", Expected: input.Len(), Actual: len(result.Errs)}
		}
		copy(errs, result.Errs)
		return nil
	})
	if err != nil {
		return EnrichResult{CallErr: errors.NewE(err)}
	}
	select {
	case <-ctx.Done():
		invocation.Cancel(ctx.Err())
		return EnrichResult{CallErr: errors.NewE(ctx.Err())}
	case <-invocation.Done():
	}
	if err := invocation.Err(); err != nil {
		return EnrichResult{CallErr: errors.NewE(err)}
	}
	return EnrichResult{Errs: errs}
}

// shadow mirrors the batch onto a shadow candidate, cut the way production cuts it so an oversized
// payload fails the same way. SubmitShadow does not block; the runtime has its own shadow budget.
func (r *Application) shadow(ctx context.Context, enrichmentID string, input *alerts.Batch, callBudget int, generation int64) {
	bounds := runtime.ChunkBounds(input.WireSizes(), runtime.MaxCallPayloadBytes, callBudget)
	for i := range len(bounds) - 1 {
		shadowInput := input.Slice(bounds[i], bounds[i+1]).Clone()
		_, _ = r.Application.SubmitShadow(ctx, enrichmentID, generation, func(callCtx context.Context, enrichment Enrichment) error {
			if !enrichment.EnrichmentMetadata().Enabled {
				return nil
			}
			result := enrichment.EnrichBatch(callCtx, shadowInput)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Errs) != shadowInput.Len() {
				return fmt.Errorf("enrichment %s returned %d errors for %d shadow alerts", enrichmentID, len(result.Errs), shadowInput.Len())
			}
			for _, err := range result.Errs {
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
}

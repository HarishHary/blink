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
func (r *Application) Enrich(ctx context.Context, state snapshot.ProjectionState[*EnrichmentMetadata], enrichmentID string, input []*alerts.Alert) EnrichResult {
	if r == nil || r.Application == nil {
		return EnrichResult{CallErr: errors.NewE(runtime.ErrRuntimeNotStarted)}
	}
	if len(input) == 0 {
		return EnrichResult{Errs: []errors.Error{}}
	}
	generation := state.CommittedGeneration
	rollout := state.RolloutByID[enrichmentID]
	callBudget := r.CallBudget(rollout)
	alertBytes := alerts.SampleWireSize(input)

	if rollout.Shadow {
		r.shadow(ctx, enrichmentID, input, callBudget, alertBytes, generation)
	}
	owned := alerts.CloneAlerts(input)

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
		group.alerts = append(group.alerts, owned[i])
	}

	parts := make([]EnrichResult, len(groups))
	workerBudget := max(1, callBudget/len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Go(func() {
			chunks := runtime.MaxChunks(len(group.alerts), workerBudget, alertBytes)
			shards := runtime.ShardPooled(group.alerts, chunks, workerBudget, func(chunk []*alerts.Alert) EnrichResult {
				return r.enrichChunk(ctx, enrichmentID, group.rolloutKey, generation, chunk)
			})
			part := EnrichResult{Errs: make([]errors.Error, 0, len(group.alerts))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = EnrichResult{CallErr: shard.CallErr}
					return
				}
				part.Errs = append(part.Errs, shard.Errs...)
			}
			parts[i] = part
		})
	}
	wg.Wait()

	result := EnrichResult{Errs: make([]errors.Error, len(input))}
	for i, part := range parts {
		if part.CallErr != nil {
			return EnrichResult{CallErr: part.CallErr}
		}
		if len(part.Errs) != len(groups[i].indexes) {
			return EnrichResult{CallErr: errors.NewF("enrichment %s returned invalid routed result shape", enrichmentID)}
		}
		for j, inputIndex := range groups[i].indexes {
			result.Errs[inputIndex] = part.Errs[j]
			if part.Errs[j] == nil {
				*input[inputIndex] = *groups[i].alerts[j]
			}
		}
	}
	return result
}

func (r *Application) enrichChunk(ctx context.Context, enrichmentID, rolloutKey string, generation int64, input []*alerts.Alert) EnrichResult {
	errs := make([]errors.Error, len(input))
	invocation, err := r.Application.Submit(ctx, enrichmentID, rolloutKey, generation, func(callCtx context.Context, enrichment Enrichment) error {
		if !enrichment.EnrichmentMetadata().Enabled {
			return nil
		}
		result := enrichment.EnrichBatch(callCtx, input)
		if result.CallErr != nil {
			return result.CallErr
		}
		if len(result.Errs) != len(input) {
			return &errors.ResultCardinalityError{PluginKind: "enrichment", PluginID: enrichmentID, Field: "errors", Expected: len(input), Actual: len(result.Errs)}
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
func (r *Application) shadow(ctx context.Context, enrichmentID string, input []*alerts.Alert, callBudget, alertBytes int, generation int64) {
	for _, chunk := range runtime.ShardSlice(input, runtime.MaxChunks(len(input), callBudget, alertBytes)) {
		shadowInput := alerts.CloneAlerts(chunk)
		_, _ = r.Application.SubmitShadow(ctx, enrichmentID, generation, func(callCtx context.Context, enrichment Enrichment) error {
			if !enrichment.EnrichmentMetadata().Enabled {
				return nil
			}
			result := enrichment.EnrichBatch(callCtx, shadowInput)
			if result.CallErr != nil {
				return result.CallErr
			}
			if len(result.Errs) != len(shadowInput) {
				return fmt.Errorf("enrichment %s returned %d errors for %d shadow alerts", enrichmentID, len(result.Errs), len(shadowInput))
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

package formatters

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	alts "github.com/harishhary/blink/pkg/alerts"
)

// Pool routes formatter calls across the live formatter process pool.
type Pool struct {
	*pools.ProcessPool[Formatter]
}

// NewPool builds the formatter pool with live rollout derived from cfg (see the
// rules pool for the closure rationale).
func NewPool(cfg config.Source[*FormatterMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Formatter](config.RolloutFor(cfg), pools.NewPoolMetrics("formatters"), drainTimeout),
	}
}

// FormatResult holds the batch-level result from formatting alerts.
type FormatResult struct {
	Outs    []map[string]any
	Errs    []errors.Error // per-alert (aligned with Outs)
	CallErr errors.Error   // whole-call failure; never alert-scoped
}

// Format runs the formatter identified by id against all alerts. When the routed pool has
// max_procs > 1 the alerts are sharded into that many contiguous chunks formatted concurrently
// (each on its own subprocess) and the per-alert outs/errs are concatenated in original order.
//   - outs/errs are per-alert (same length as alertBatch) on success.
func (p *Pool) Format(ctx context.Context, formatterID string, alerts []*alts.Alert, canaryHashKey string) FormatResult {
	p.shadowFormat(ctx, formatterID, alerts)
	k := p.ServingPoolSize(formatterID, canaryHashKey)
	parts := pools.ShardConcurrent(alerts, k, func(chunk []*alts.Alert) FormatResult {
		return p.formatChunk(ctx, formatterID, chunk, canaryHashKey)
	})
	result := FormatResult{
		Outs: make([]map[string]any, 0, len(alerts)),
		Errs: make([]errors.Error, 0, len(alerts)),
	}
	for _, part := range parts {
		if part.CallErr != nil {
			return FormatResult{CallErr: part.CallErr}
		}
		result.Outs = append(result.Outs, part.Outs...)
		result.Errs = append(result.Errs, part.Errs...)
	}
	return result
}

func (p *Pool) formatChunk(ctx context.Context, formatterID string, altsChunk []*alts.Alert, canaryHashKey string) FormatResult {
	outs := make([]map[string]any, len(altsChunk))
	perErrs := make([]errors.Error, len(altsChunk))
	prodFn := func(callCtx context.Context, f Formatter) error {
		if !f.FormatterMetadata().Enabled {
			return nil
		}
		batchResult := f.FormatBatch(callCtx, altsChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Outs) != len(altsChunk) {
			return &errors.ResultCardinalityError{PluginKind: "formatter", PluginID: formatterID, Field: "results", Expected: len(altsChunk), Actual: len(batchResult.Outs)}
		}
		if len(batchResult.Errs) != len(altsChunk) {
			return &errors.ResultCardinalityError{PluginKind: "formatter", PluginID: formatterID, Field: "errors", Expected: len(altsChunk), Actual: len(batchResult.Errs)}
		}
		copy(outs, batchResult.Outs)
		copy(perErrs, batchResult.Errs)
		return nil
	}
	err := p.Call(ctx, formatterID, canaryHashKey, prodFn)
	if err != nil {
		return FormatResult{CallErr: errors.NewE(err)}
	}
	return FormatResult{Outs: outs, Errs: perErrs}
}

// shadowFormat fans the full batch out to the shadow candidate (if formatterID is in shadow mode) at
// its own max_procs, each shard a detached CallShadow whose result is dropped. Format is read-only on
// the alerts (returns new maps), so shards share the batch.
func (p *Pool) shadowFormat(ctx context.Context, formatterID string, alerts []*alts.Alert) {
	sk := p.ShadowPoolSize(formatterID)
	if sk == 0 || len(alerts) == 0 {
		return
	}
	for _, altsChunk := range pools.ShardSlice(alerts, sk) {
		p.CallShadow(ctx, formatterID, func(callCtx context.Context, f Formatter) error {
			if !f.FormatterMetadata().Enabled {
				return nil
			}
			result := f.FormatBatch(callCtx, altsChunk)
			if result.CallErr != nil {
				return result.CallErr
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

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

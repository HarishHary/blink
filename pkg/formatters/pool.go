package formatters

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	alts "github.com/harishhary/blink/pkg/alerts"
)

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

type formatChunkResult struct {
	outs    []map[string]any
	absent  bool
	removed bool
	errs    []errors.Error // per-alert (aligned with the chunk)
	callErr errors.Error   // whole-call failure (not per-alert)
}

// Format runs the formatter identified by id against all alerts. When the routed pool has
// max_procs > 1 the alerts are sharded into that many contiguous chunks formatted concurrently
// (each on its own subprocess) and the per-alert outs/errs are concatenated in original order.
//   - absent=true: plugin transiently missing, caller should dead-letter.
//   - removed=true: plugin deregistered, caller should drop permanently.
//   - outs/errs are per-alert (same length as alertBatch) on success.
func (p *Pool) Format(ctx context.Context, formatterID string, alerts []*alts.Alert, canaryHashKey string) (outs []map[string]any, absent bool, removed bool, errs []errors.Error) {
	p.shadowFormat(ctx, formatterID, alerts)
	k := p.ServingPoolSize(formatterID, canaryHashKey)
	parts := pools.ShardConcurrent(alerts, k, func(chunk []*alts.Alert) formatChunkResult {
		return p.formatChunk(ctx, formatterID, chunk, canaryHashKey)
	})

	// Pool-level conditions apply to the whole batch (every shard hit the same routed pool).
	for _, part := range parts {
		if part.removed {
			return nil, false, true, nil
		}
		if part.absent {
			return nil, true, false, nil
		}
		if part.callErr != nil {
			return nil, false, false, []errors.Error{part.callErr}
		}
	}

	outs = make([]map[string]any, 0, len(alerts))
	errs = make([]errors.Error, 0, len(alerts))
	for _, part := range parts {
		outs = append(outs, part.outs...)
		errs = append(errs, part.errs...)
	}
	return outs, false, false, errs
}

func (p *Pool) formatChunk(ctx context.Context, formatterID string, altsChunk []*alts.Alert, canaryHashKey string) formatChunkResult {
	outs := make([]map[string]any, len(altsChunk))
	perErrs := make([]errors.Error, len(altsChunk))
	prodFn := func(callCtx context.Context, f Formatter) error {
		if !f.FormatterMetadata().Enabled {
			return nil
		}
		batchOuts, e := f.Format(callCtx, altsChunk)
		if e != nil {
			for i := range perErrs {
				perErrs[i] = e
			}
			return nil
		}
		copy(outs, batchOuts)
		return nil
	}
	err := p.Call(ctx, formatterID, canaryHashKey, prodFn)
	if err != nil {
		if stderrors.Is(err, pools.ErrPluginNotFound) {
			return formatChunkResult{absent: true}
		}
		if stderrors.Is(err, pools.ErrPluginRemoved) {
			return formatChunkResult{removed: true}
		}
		return formatChunkResult{callErr: errors.NewE(err)}
	}
	return formatChunkResult{outs: outs, errs: perErrs}
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
			_, e := f.Format(callCtx, altsChunk)
			return e
		})
	}
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }

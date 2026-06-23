package formatters

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/messaging"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
)

type Pool struct {
	*internal.ProcessPool[Formatter]
}

func NewPool(routing *internal.RoutingTable, drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: internal.NewProcessPool[Formatter](routing.Config(), internal.NewPoolMetrics("formatters"), drainTimeout),
	}
}

// Format runs the formatter identified by formatterID against all alerts in a single pool call.
//   - absent=true: plugin transiently missing, caller should dead-letter.
//   - removed=true: plugin deregistered, caller should drop permanently.
//   - outs/errs are per-alert (same length as alerts).
func (p *Pool) Format(ctx context.Context, formatterID string, alerts []*alerts.Alert, canaryHashKey string) (outs []map[string]any, absent bool, removed bool, errs []errors.Error) {
	outs = make([]map[string]any, len(alerts))
	errs = make([]errors.Error, len(alerts))
	err := p.Call(ctx, formatterID, canaryHashKey, func(callCtx context.Context, f Formatter) error {
		if !f.FormatterMetadata().Enabled {
			return nil
		}
		batchOuts, e := f.Format(callCtx, alerts)
		if e != nil {
			for i := range errs {
				errs[i] = e
			}
			return nil
		}
		copy(outs, batchOuts)
		return nil
	})
	if err != nil {
		if stderrors.Is(err, internal.ErrPluginNotFound) {
			return nil, true, false, nil
		}
		if stderrors.Is(err, internal.ErrPluginRemoved) {
			return nil, false, true, nil
		}
		return nil, false, false, []errors.Error{errors.NewE(err)}
	}
	return outs, false, false, errs
}

func poolKey(f Formatter) internal.PoolKey {
	cfg := f.FormatterMetadata()
	return internal.PoolKey{Id: cfg.Id, Name: cfg.Name, Hash: f.Checksum()}
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []Formatter, maxProcs int) {
		p.Register(poolKey(items[0]), items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case executor.RegisterMessage[Formatter]:
		register(nil, m.Items, m.MaxProcs)
	case executor.UpdateMessage[Formatter]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case executor.UnregisterMessage[Formatter]:
		p.Unregister(m.ItemKey)
	case executor.RemoveMessage[Formatter]:
		p.Remove(m.ItemKey)
	case executor.MigrateMessage[Formatter]:
		p.MigrateSlots(m.ActiveKey.Id, m.ActiveKey, m.PendingKey)
	}
}

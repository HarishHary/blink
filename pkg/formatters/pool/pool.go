package pool

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/pluginmgr"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters"
)

type Pool struct {
	*internal.ProcessPool[formatters.IFormatter]
}

func NewPool(routing *internal.RoutingTable, drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: internal.NewProcessPool[formatters.IFormatter](routing.Config(), internal.NewPoolMetrics("formatters"), drainTimeout),
	}
}

// Format runs the formatter identified by formatterID against all alerts in a single pool call.
//   - absent=true: plugin transiently missing, caller should dead-letter.
//   - removed=true: plugin deregistered, caller should drop permanently.
//   - outs/errs are per-alert (same length as alerts).
func (p *Pool) Format(ctx context.Context, formatterID string, alerts []*alerts.Alert, canaryHashKey string) (outs []map[string]any, absent bool, removed bool, errs []errors.Error) {
	outs = make([]map[string]any, len(alerts))
	errs = make([]errors.Error, len(alerts))
	err := p.Call(ctx, formatterID, canaryHashKey, func(callCtx context.Context, f formatters.IFormatter) error {
		if !f.Enabled() {
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

func poolKey(f formatters.IFormatter) internal.PoolKey {
	version := f.Version()
	if cs := f.Checksum(); cs != "" {
		version = version + "@" + cs
	}
	return internal.PoolKey{PluginID: f.Id(), Version: version}
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []formatters.IFormatter, maxProcs int) {
		p.Register(poolKey(items[0]), items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case pluginmgr.RegisterMessage[formatters.IFormatter]:
		register(nil, m.Items, m.MaxProcs)
	case pluginmgr.UpdateMessage[formatters.IFormatter]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case pluginmgr.UnregisterMessage[formatters.IFormatter]:
		p.Unregister(m.ItemID)
	case pluginmgr.RemoveMessage[formatters.IFormatter]:
		p.Remove(m.ItemID)
	}
}

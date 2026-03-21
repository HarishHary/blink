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

// Format runs the formatter identified by formatterID against alert.
// It respects kill switches and the rollout mode from the current catalog snapshot.
//   - absent=true: no active pool - plugin transiently missing, caller should dead-letter.
//   - removed=true: plugin was explicitly deregistered, caller should drop permanently.
func (p *Pool) Format(ctx context.Context, formatterID string, alert *alerts.Alert, canaryHashKey string) (out map[string]any, absent bool, removed bool, _ errors.Error) {
	err := p.Call(ctx, formatterID, canaryHashKey, func(callCtx context.Context, f formatters.IFormatter) error {
		if !f.Enabled() {
			return nil
		}
		var e errors.Error
		out, e = f.Format(callCtx, alert)
		return e
	})
	if err != nil {
		if stderrors.Is(err, internal.ErrPluginNotFound) {
			return nil, true, false, nil
		}
		if stderrors.Is(err, internal.ErrPluginRemoved) {
			return nil, false, true, nil
		}
		return nil, false, false, errors.NewE(err)
	}
	return out, false, false, nil
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []formatters.IFormatter, maxProcs int) {
		version := items[0].Checksum()
		if version == "" {
			version = "1.0.0"
		}
		p.Register(internal.PoolKey{PluginID: items[0].Id(), Version: version}, items, maxProcs, onDrained)
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

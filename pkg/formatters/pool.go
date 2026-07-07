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
	"github.com/harishhary/blink/pkg/alerts"
)

type Pool struct {
	*pools.ProcessPool[Formatter]
}

// NewPool builds the formatter pool with live rollout routing derived from cfg (see the
// rules pool for the closure rationale).
func NewPool(cfg config.Source[*FormatterMetadata], drainTimeout time.Duration) *Pool {
	routing := func(id, name string) (pools.RolloutMode, float64) {
		if name != "" {
			if m, ok := cfg.ByFileName(name); ok {
				return m.RolloutMode, m.RolloutPct
			}
		}
		re := cfg.RoutingByID(id)
		return re.Mode, re.RolloutPct
	}
	return &Pool{
		ProcessPool: pools.NewProcessPool[Formatter](routing, pools.NewPoolMetrics("formatters"), drainTimeout),
	}
}

// Format runs the formatter identified by id against all alerts in a single pool call.
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
		if stderrors.Is(err, pools.ErrPluginNotFound) {
			return nil, true, false, nil
		}
		if stderrors.Is(err, pools.ErrPluginRemoved) {
			return nil, false, true, nil
		}
		return nil, false, false, []errors.Error{errors.NewE(err)}
	}
	return outs, false, false, errs
}

func poolKey(f Formatter) pools.PoolKey {
	cfg := f.FormatterMetadata()
	return pools.PoolKey{Id: cfg.Id, Name: cfg.Name, Hash: f.Checksum()}
}

func (p *Pool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case plugin.RegisterMessage[Formatter]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, nil)
	case plugin.UpdateMessage[Formatter]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, m.OnDrained)
	case plugin.UnregisterMessage[Formatter]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[Formatter]:
		p.Remove(m.ItemKey)
	case plugin.MigrateMessage[Formatter]:
		p.MigrateSlots(m.ActiveKey.Id, m.ActiveKey, m.PendingKey)
	}
}

package pool

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/pluginmgr"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
	tuning "github.com/harishhary/blink/pkg/tuning_rules"
)

type Pool struct {
	*internal.ProcessPool[tuning.TuningRule]
}

func NewPool(routing *internal.RoutingTable, drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: internal.NewProcessPool[tuning.TuningRule](routing.Config(), internal.NewPoolMetrics("tuning_rules"), drainTimeout),
	}
}

// Runs the tuning rule identified by tuningRuleID against alert.
func (p *Pool) Tune(ctx context.Context, tuningRuleID string, alert alerts.Alert, canaryHashKey string) (bool, errors.Error) {
	var matched bool
	err := p.Call(ctx, tuningRuleID, canaryHashKey, func(callCtx context.Context, t tuning.TuningRule) error {
		var e errors.Error
		matched, e = t.Tune(callCtx, alert)
		return e
	})
	if err != nil {
		return false, errors.NewE(err)
	}
	return matched, nil
}

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering tuning rules in the pool.
func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []tuning.TuningRule, maxProcs int) {
		version := items[0].Checksum()
		if version == "" {
			version = "1.0.0"
		}
		p.Register(internal.PoolKey{PluginID: items[0].Id(), Version: version}, items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case pluginmgr.RegisterMessage[tuning.TuningRule]:
		register(nil, m.Items, m.MaxProcs)
	case pluginmgr.UpdateMessage[tuning.TuningRule]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case pluginmgr.UnregisterMessage[tuning.TuningRule]:
		p.Unregister(m.ItemID)
	case pluginmgr.RemoveMessage[tuning.TuningRule]:
		p.Remove(m.ItemID)
	}
}

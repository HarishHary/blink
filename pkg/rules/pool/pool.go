package pool

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/pluginmgr"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/rules/config"
)

type Pool struct {
	*internal.ProcessPool[rules.Rule]
	watcher *config.Watcher
}

func NewPool(watcher *config.Watcher, drainTimeout time.Duration) *Pool {
	routing := func(id string) (bool, internal.RolloutMode, float64) {
		meta := watcher.Current().ByID(id)
		if meta == nil {
			return false, internal.RolloutModeBlueGreen, 0
		}
		return meta.KillSwitch(), meta.RolloutMode(), meta.RolloutPct()
	}
	return &Pool{
		ProcessPool: internal.NewProcessPool[rules.Rule](routing, internal.NewPoolMetrics("rules"), drainTimeout),
		watcher:     watcher,
	}
}

// Runs the rule identified by ruleID against event.
func (p *Pool) Evaluate(ctx context.Context, ruleID string, event events.Event, canaryHashKey string) (bool, errors.Error) {
	var matched bool
	err := p.Call(ctx, ruleID, canaryHashKey, func(ctx context.Context, r rules.Rule) error {
		if !r.Enabled() {
			return nil
		}
		var e errors.Error
		matched, e = r.Evaluate(ctx, event)
		return e
	})
	if err != nil {
		return false, errors.NewE(err)
	}
	return matched, nil
}

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering rules in the pool.
func (p *Pool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case pluginmgr.RegisterMessage[rules.Rule]:
		r := m.Items[0]
		p.Register(internal.PoolKey{PluginID: r.Id(), Version: r.Version()}, m.Items, m.MaxProcs, nil)
	case pluginmgr.UpdateMessage[rules.Rule]:
		r := m.Items[0]
		p.Register(internal.PoolKey{PluginID: r.Id(), Version: r.Version()}, m.Items, m.MaxProcs, m.OnDrained)
	case pluginmgr.UnregisterMessage[rules.Rule]:
		p.Unregister(m.ItemID)
	case pluginmgr.RemoveMessage[rules.Rule]:
		p.Remove(m.ItemID)
	}
}

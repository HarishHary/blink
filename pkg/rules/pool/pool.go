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
	routing := func(id string) (internal.RolloutMode, float64) {
		re := watcher.Current().RoutingByID(id)
		return re.Mode, re.RolloutPct
	}
	return &Pool{
		ProcessPool: internal.NewProcessPool[rules.Rule](routing, internal.NewPoolMetrics("rules"), drainTimeout),
		watcher:     watcher,
	}
}

// Evaluate runs all evts against the rule identified by ruleID in a single pool call.
func (p *Pool) Evaluate(ctx context.Context, ruleID string, evts []events.Event, canaryHashKey string) ([]bool, errors.Error) {
	var results []bool
	err := p.Call(ctx, ruleID, canaryHashKey, func(ctx context.Context, r rules.Rule) error {
		if !r.Enabled() {
			results = make([]bool, len(evts))
			return nil
		}
		var e errors.Error
		results, e = r.Evaluate(ctx, evts)
		return e
	})
	if err != nil {
		return nil, errors.NewE(err)
	}
	return results, nil
}

// poolKey builds a PoolKey that is unique per binary deployment.
// Combining the YAML version with the binary checksum means a binary change
// always produces a distinct key even if the operator forgot to bump the version
// string in the rule config - preventing silent same-key overwrites in the pool.
func poolKey(r rules.Rule) internal.PoolKey {
	version := r.Version()
	if cs := r.Checksum(); cs != "" {
		version = version + "@" + cs
	}
	return internal.PoolKey{PluginID: r.Id(), Version: version}
}

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering rules in the pool.
func (p *Pool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case pluginmgr.RegisterMessage[rules.Rule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, nil)
	case pluginmgr.UpdateMessage[rules.Rule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, m.OnDrained)
	case pluginmgr.UnregisterMessage[rules.Rule]:
		p.Unregister(m.ItemID)
	case pluginmgr.RemoveMessage[rules.Rule]:
		p.Remove(m.ItemID)
	}
}

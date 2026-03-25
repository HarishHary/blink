package rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/events"
)

type Pool struct {
	*internal.ProcessPool[Rule]
}

func NewPool(manager *RuleConfigManager, drainTimeout time.Duration) *Pool {
	routing := func(id string) (internal.RolloutMode, float64) {
		re := manager.Current().RoutingByID(id)
		return re.Mode, re.RolloutPct
	}
	return &Pool{
		ProcessPool: internal.NewProcessPool[Rule](routing, internal.NewPoolMetrics("rules"), drainTimeout),
	}
}

// Evaluate runs all evts against the rule identified by ruleID in a single pool call.
func (p *Pool) Evaluate(ctx context.Context, ruleID string, evts []events.Event, canaryHashKey string) ([]EvalResult, errors.Error) {
	var results []EvalResult
	err := p.Call(ctx, ruleID, canaryHashKey, func(ctx context.Context, r Rule) error {
		if !r.RuleMetadata().Enabled {
			results = make([]EvalResult, len(evts))
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
func poolKey(r Rule) internal.PoolKey {
	cfg := r.RuleMetadata()
	return internal.PoolKey{Id: cfg.Id, Version: cfg.Version, Hash: r.Checksum()}
}

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering rules in the pool.
func (p *Pool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case plugin.RegisterMessage[Rule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, nil)
	case plugin.UpdateMessage[Rule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, m.OnDrained)
	case plugin.UnregisterMessage[Rule]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[Rule]:
		p.Remove(m.ItemKey)
	}
}

package rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/events"
)

type RulePool struct {
	*pools.ProcessPool[Rule]
}

func NewRulePool(cfg config.Source[*RuleMetadata], drainTimeout time.Duration) *RulePool {
	routing := func(id, name string) (pools.RolloutMode, float64) {
		if name != "" {
			// Register time: use per-binary spec so a stable update isn't misrouted
			// to the pending slot because a running shadow inflates the merged mode.
			if m, ok := cfg.ByFileName(name); ok {
				return m.RolloutMode, m.RolloutPct
			}
		}
		re := cfg.RoutingByID(id)
		return re.Mode, re.RolloutPct
	}
	return &RulePool{
		ProcessPool: pools.NewProcessPool[Rule](routing, pools.NewPoolMetrics("rules"), drainTimeout),
	}
}

// Evaluate runs all evts against the rule identified by ruleID in a single pool call.
func (p *RulePool) Evaluate(ctx context.Context, ruleID string, evts []events.Event, canaryHashKey string) ([]EvalResult, errors.Error) {
	var results []EvalResult
	err := p.Call(ctx, ruleID, canaryHashKey, func(callCtx context.Context, r Rule) error {
		if !r.RuleMetadata().Enabled {
			results = make([]EvalResult, len(evts))
			return nil
		}
		var e errors.Error
		results, e = r.Evaluate(callCtx, evts)
		return e
	})
	if err != nil {
		return nil, errors.NewE(err)
	}
	return results, nil
}

func poolKey(r Rule) pools.PoolKey {
	cfg := r.RuleMetadata()
	return pools.PoolKey{Id: cfg.Id, Name: cfg.Name, Hash: r.Checksum()}
}

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering rules in the pool.
func (p *RulePool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case plugin.RegisterMessage[Rule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, nil)
	case plugin.UpdateMessage[Rule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, m.OnDrained)
	case plugin.UnregisterMessage[Rule]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[Rule]:
		p.Remove(m.ItemKey)
	case plugin.MigrateMessage[Rule]:
		p.MigrateSlots(m.ActiveKey.Id, m.ActiveKey, m.PendingKey)
	}
}

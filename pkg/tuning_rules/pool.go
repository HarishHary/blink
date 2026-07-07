package tuning_rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
)

type Pool struct {
	*pools.ProcessPool[TuningRule]
}

// NewPool builds the tuning-rule pool with live rollout routing derived from src (see the
// rules pool for the closure rationale).
func NewPool(src config.Source[*TuningRuleMetadata], drainTimeout time.Duration) *Pool {
	routing := func(id, name string) (pools.RolloutMode, float64) {
		if name != "" {
			if m, ok := src.ByFileName(name); ok {
				return m.RolloutMode, m.RolloutPct
			}
		}
		re := src.RoutingByID(id)
		return re.Mode, re.RolloutPct
	}
	return &Pool{
		ProcessPool: pools.NewProcessPool[TuningRule](routing, pools.NewPoolMetrics("tuning_rules"), drainTimeout),
	}
}

// Tune calls tuningRuleID once with all alerts, returning per-alert apply results.
// ruleType and confidence are rule metadata - the same for every alert in the batch.
func (p *Pool) Tune(ctx context.Context, tuningRuleID string, alerts []alerts.Alert, canaryHashKey string) (
	ruleType RuleType, confidence scoring.Confidence, applies []bool, _ errors.Error,
) {
	applies = make([]bool, len(alerts))
	err := p.Call(ctx, tuningRuleID, canaryHashKey, func(callCtx context.Context, t TuningRule) error {
		if !t.TuningRuleMetadata().Enabled {
			return nil
		}
		ruleType = t.RuleType()
		confidence = t.Confidence()
		var e errors.Error
		applies, e = t.Tune(callCtx, alerts)
		return e
	})
	if err != nil {
		return 0, 0, nil, errors.NewE(err)
	}
	return ruleType, confidence, applies, nil
}

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering tuning rules in the pool.
func poolKey(t TuningRule) pools.PoolKey {
	cfg := t.TuningRuleMetadata()
	return pools.PoolKey{Id: cfg.Id, Name: cfg.Name, Hash: t.Checksum()}
}

func (p *Pool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case plugin.RegisterMessage[TuningRule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, nil)
	case plugin.UpdateMessage[TuningRule]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, m.OnDrained)
	case plugin.UnregisterMessage[TuningRule]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[TuningRule]:
		p.Remove(m.ItemKey)
	case plugin.MigrateMessage[TuningRule]:
		p.MigrateSlots(m.ActiveKey.Id, m.ActiveKey, m.PendingKey)
	}
}

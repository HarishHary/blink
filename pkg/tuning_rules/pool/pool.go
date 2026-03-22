package pool

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/pluginmgr"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
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

// Tune calls tuningRuleID once with all alerts, returning per-alert apply results.
// ruleType and confidence are rule metadata - the same for every alert in the batch.
func (p *Pool) Tune(ctx context.Context, tuningRuleID string, alerts []alerts.Alert, canaryHashKey string) (
	ruleType tuning.RuleType, confidence scoring.Confidence, applies []bool, _ errors.Error,
) {
	applies = make([]bool, len(alerts))
	err := p.Call(ctx, tuningRuleID, canaryHashKey, func(callCtx context.Context, t tuning.TuningRule) error {
		if !t.Enabled() {
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
func poolKey(t tuning.TuningRule) internal.PoolKey {
	version := t.Version()
	if cs := t.Checksum(); cs != "" {
		version = version + "@" + cs
	}
	return internal.PoolKey{PluginID: t.Id(), Version: version}
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []tuning.TuningRule, maxProcs int) {
		p.Register(poolKey(items[0]), items, maxProcs, onDrained)
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

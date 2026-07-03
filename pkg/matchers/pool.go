package matchers

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

type MatcherPool struct {
	*pools.ProcessPool[Matcher]
}

// NewMatcherPool builds the matcher pool with live rollout routing derived from cfg: a
// running canary/shadow artifact's mode+pct comes from its own spec (by binary name);
// otherwise the merged per-ID routing applies. Mirrors the rules pool.
func NewMatcherPool(cfg config.Source[*MatcherMetadata], drainTimeout time.Duration) *MatcherPool {
	routing := func(id, name string) (pools.RolloutMode, float64) {
		if name != "" {
			if m, ok := cfg.ByFileName(name); ok {
				return m.RolloutMode, m.RolloutPct
			}
		}
		re := cfg.RoutingByID(id)
		return re.Mode, re.RolloutPct
	}
	return &MatcherPool{
		ProcessPool: pools.NewProcessPool[Matcher](routing, pools.NewPoolMetrics("matchers"), drainTimeout),
	}
}

// Match runs the matcher identified by matcherID against all events in a single pool call.
// Disabled matchers are treated as pass-through (all results true).
func (p *MatcherPool) Match(ctx context.Context, matcherID string, evts []events.Event, canaryHashKey string) ([]bool, errors.Error) {
	var results []bool
	err := p.Call(ctx, matcherID, canaryHashKey, func(callCtx context.Context, m Matcher) error {
		if !m.MatcherMetadata().Enabled {
			results = make([]bool, len(evts))
			for i := range results {
				results[i] = true
			}
			return nil
		}
		var e errors.Error
		results, e = m.Match(callCtx, evts)
		return e
	})
	if err != nil {
		return nil, errors.NewE(err)
	}
	return results, nil
}

func poolKey(m Matcher) pools.PoolKey {
	cfg := m.MatcherMetadata()
	return pools.PoolKey{Id: cfg.Id, Name: cfg.Name, Hash: m.Checksum()}
}

func (p *MatcherPool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case plugin.RegisterMessage[Matcher]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, nil)
	case plugin.UpdateMessage[Matcher]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, m.OnDrained)
	case plugin.UnregisterMessage[Matcher]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[Matcher]:
		p.Remove(m.ItemKey)
	case plugin.MigrateMessage[Matcher]:
		p.MigrateSlots(m.ActiveKey.Id, m.ActiveKey, m.PendingKey)
	}
}

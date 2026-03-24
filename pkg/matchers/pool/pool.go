package pool

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

type Pool struct {
	*internal.ProcessPool[matchers.Matcher]
}

func NewPool(routing *internal.RoutingTable, drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: internal.NewProcessPool[matchers.Matcher](routing.Config(), internal.NewPoolMetrics("matchers"), drainTimeout),
	}
}

// Match runs the matcher identified by matcherID against all events in a single pool call.
// Disabled matchers are treated as pass-through (all results true).
func (p *Pool) Match(ctx context.Context, matcherID string, evts []events.Event, canaryHashKey string) ([]bool, errors.Error) {
	var results []bool
	err := p.Call(ctx, matcherID, canaryHashKey, func(callCtx context.Context, m matchers.Matcher) error {
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

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering matchers in the pool.
func poolKey(m matchers.Matcher) internal.PoolKey {
	cfg := m.MatcherMetadata()
	return internal.PoolKey{Id: cfg.Id, Version: cfg.Version, Hash: m.Checksum()}
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []matchers.Matcher, maxProcs int) {
		p.Register(poolKey(items[0]), items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case plugin.RegisterMessage[matchers.Matcher]:
		register(nil, m.Items, m.MaxProcs)
	case plugin.UpdateMessage[matchers.Matcher]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case plugin.UnregisterMessage[matchers.Matcher]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[matchers.Matcher]:
		p.Remove(m.ItemKey)
	}
}

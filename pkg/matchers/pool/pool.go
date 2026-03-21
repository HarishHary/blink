package pool

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/pluginmgr"
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

// Runs the matcher identified by matcherID against event.
func (p *Pool) Match(ctx context.Context, matcherID string, event events.Event, canaryHashKey string) (bool, errors.Error) {
	var matched bool
	err := p.Call(ctx, matcherID, canaryHashKey, func(callCtx context.Context, m matchers.Matcher) error {
		if !m.Enabled() {
			matched = true // treat disabled matcher as pass-through
			return nil
		}
		var e errors.Error
		matched, e = m.Match(callCtx, event)
		return e
	})
	if err != nil {
		return false, errors.NewE(err)
	}
	return matched, nil
}

// Handles plugin lifecycle messages from the plugin manager bus, registering or deregistering matchers in the pool.
func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []matchers.Matcher, maxProcs int) {
		version := items[0].Checksum()
		if version == "" {
			version = "1.0.0"
		}
		p.Register(internal.PoolKey{PluginID: items[0].Id(), Version: version}, items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case pluginmgr.RegisterMessage[matchers.Matcher]:
		register(nil, m.Items, m.MaxProcs)
	case pluginmgr.UpdateMessage[matchers.Matcher]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case pluginmgr.UnregisterMessage[matchers.Matcher]:
		p.Unregister(m.ItemID)
	case pluginmgr.RemoveMessage[matchers.Matcher]:
		p.Remove(m.ItemID)
	}
}

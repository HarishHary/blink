package pool

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/pluginmgr"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments"
)

type Pool struct {
	*internal.ProcessPool[enrichments.IEnrichment]
}

func NewPool(routing *internal.RoutingTable, drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: internal.NewProcessPool[enrichments.IEnrichment](routing.Config(), internal.NewPoolMetrics("enrichments"), drainTimeout),
	}
}

func (p *Pool) Enrich(ctx context.Context, enrichmentID string, alert *alerts.Alert, canaryHashKey string) (absent bool, removed bool, _ errors.Error) {
	err := p.Call(ctx, enrichmentID, canaryHashKey, func(ctx context.Context, e enrichments.IEnrichment) error {
		if !e.Enabled() {
			return nil
		}
		return e.Enrich(ctx, alert)
	})
	if err != nil {
		if stderrors.Is(err, internal.ErrPluginNotFound) {
			return true, false, nil
		}
		if stderrors.Is(err, internal.ErrPluginRemoved) {
			return false, true, nil
		}
		return false, false, errors.NewE(err)
	}
	return false, false, nil
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []enrichments.IEnrichment, maxProcs int) {
		version := items[0].Checksum()
		if version == "" {
			version = "1.0.0"
		}
		p.Register(internal.PoolKey{PluginID: items[0].Id(), Version: version}, items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case pluginmgr.RegisterMessage[enrichments.IEnrichment]:
		register(nil, m.Items, m.MaxProcs)
	case pluginmgr.UpdateMessage[enrichments.IEnrichment]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case pluginmgr.UnregisterMessage[enrichments.IEnrichment]:
		p.Unregister(m.ItemID)
	case pluginmgr.RemoveMessage[enrichments.IEnrichment]:
		p.Remove(m.ItemID)
	}
}

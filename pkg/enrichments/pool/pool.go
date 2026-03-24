package pool

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments"
)

type Pool struct {
	*internal.ProcessPool[enrichments.Enrichment]
}

func NewPool(routing *internal.RoutingTable, drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: internal.NewProcessPool[enrichments.Enrichment](routing.Config(), internal.NewPoolMetrics("enrichments"), drainTimeout),
	}
}

// Enrich calls enrichmentID once with all alerts, applying enrichment sequentially.
// absent/removed refer to the plugin state. errs contains per-alert errors (nil on success).
func (p *Pool) Enrich(ctx context.Context, enrichmentID string, alerts []*alerts.Alert, canaryHashKey string) (absent bool, removed bool, errs []errors.Error) {
	errs = make([]errors.Error, len(alerts))
	err := p.Call(ctx, enrichmentID, canaryHashKey, func(callCtx context.Context, e enrichments.Enrichment) error {
		if !e.EnrichmentMetadata().Enabled() {
			return nil
		}
		if err := e.Enrich(callCtx, alerts); err != nil {
			for i := range errs {
				errs[i] = errors.NewE(err)
			}
		}
		return nil
	})
	if err != nil {
		if stderrors.Is(err, internal.ErrPluginNotFound) {
			return true, false, nil
		}
		if stderrors.Is(err, internal.ErrPluginRemoved) {
			return false, true, nil
		}
		return false, false, []errors.Error{errors.NewE(err)}
	}
	return false, false, errs
}

func poolKey(e enrichments.Enrichment) internal.PoolKey {
	cfg := e.EnrichmentMetadata()
	version := cfg.Version()
	if cs := e.Checksum(); cs != "" {
		version = version + "@" + cs
	}
	return internal.PoolKey{PluginID: cfg.Id(), Version: version}
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []enrichments.Enrichment, maxProcs int) {
		p.Register(poolKey(items[0]), items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case plugin.RegisterMessage[enrichments.Enrichment]:
		register(nil, m.Items, m.MaxProcs)
	case plugin.UpdateMessage[enrichments.Enrichment]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case plugin.UnregisterMessage[enrichments.Enrichment]:
		p.Unregister(m.ItemID)
	case plugin.RemoveMessage[enrichments.Enrichment]:
		p.Remove(m.ItemID)
	}
}

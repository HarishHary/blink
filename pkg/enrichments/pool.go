package enrichments

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
)

type Pool struct {
	*internal.ProcessPool[Enrichment]
}

func NewPool(routing *internal.RoutingTable, drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: internal.NewProcessPool[Enrichment](routing.Config(), internal.NewPoolMetrics("enrichments"), drainTimeout),
	}
}

// Enrich calls enrichmentID once with all alerts, applying enrichment sequentially.
// absent/removed refer to the plugin state. errs contains per-alert errors (nil on success).
func (p *Pool) Enrich(ctx context.Context, enrichmentID string, alerts []*alerts.Alert, canaryHashKey string) (absent bool, removed bool, errs []errors.Error) {
	errs = make([]errors.Error, len(alerts))
	err := p.Call(ctx, enrichmentID, canaryHashKey, func(callCtx context.Context, e Enrichment) error {
		if !e.EnrichmentMetadata().Enabled {
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

func poolKey(e Enrichment) internal.PoolKey {
	cfg := e.EnrichmentMetadata()
	return internal.PoolKey{Id: cfg.Id, Version: cfg.Version, Hash: e.Checksum()}
}

func (p *Pool) Sync(msg messaging.Message) {
	register := func(onDrained func(), items []Enrichment, maxProcs int) {
		p.Register(poolKey(items[0]), items, maxProcs, onDrained)
	}
	switch m := msg.(type) {
	case plugin.RegisterMessage[Enrichment]:
		register(nil, m.Items, m.MaxProcs)
	case plugin.UpdateMessage[Enrichment]:
		register(m.OnDrained, m.Items, m.MaxProcs)
	case plugin.UnregisterMessage[Enrichment]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[Enrichment]:
		p.Remove(m.ItemKey)
	}
}

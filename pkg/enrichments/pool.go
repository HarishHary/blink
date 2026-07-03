package enrichments

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/alerts"
)

type EnrichmentPool struct {
	*pools.ProcessPool[Enrichment]
}

// NewEnrichmentPool builds the enrichment pool with live rollout routing derived from src (see the
// rules pool for the closure rationale).
func NewEnrichmentPool(src config.Source[*EnrichmentMetadata], drainTimeout time.Duration) *EnrichmentPool {
	routing := func(id, name string) (pools.RolloutMode, float64) {
		if name != "" {
			if m, ok := src.ByFileName(name); ok {
				return m.RolloutMode, m.RolloutPct
			}
		}
		re := src.RoutingByID(id)
		return re.Mode, re.RolloutPct
	}
	return &EnrichmentPool{
		ProcessPool: pools.NewProcessPool[Enrichment](routing, pools.NewPoolMetrics("enrichments"), drainTimeout),
	}
}

// Enrich calls enrichmentID once with all alerts, applying enrichment sequentially.
// absent/removed refer to the plugin state. errs contains per-alert errors (nil on success).
func (p *EnrichmentPool) Enrich(ctx context.Context, enrichmentID string, alerts []*alerts.Alert, canaryHashKey string) (absent bool, removed bool, errs []errors.Error) {
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
		if stderrors.Is(err, pools.ErrPluginNotFound) {
			return true, false, nil
		}
		if stderrors.Is(err, pools.ErrPluginRemoved) {
			return false, true, nil
		}
		return false, false, []errors.Error{errors.NewE(err)}
	}
	return false, false, errs
}

func poolKey(e Enrichment) pools.PoolKey {
	cfg := e.EnrichmentMetadata()
	return pools.PoolKey{Id: cfg.Id, Name: cfg.Name, Hash: e.Checksum()}
}

func (p *EnrichmentPool) Sync(msg messaging.Message) {
	switch m := msg.(type) {
	case plugin.RegisterMessage[Enrichment]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, nil)
	case plugin.UpdateMessage[Enrichment]:
		p.Register(poolKey(m.Items[0]), m.Items, m.MaxProcs, m.OnDrained)
	case plugin.UnregisterMessage[Enrichment]:
		p.Unregister(m.ItemKey)
	case plugin.RemoveMessage[Enrichment]:
		p.Remove(m.ItemKey)
	case plugin.MigrateMessage[Enrichment]:
		p.MigrateSlots(m.ActiveKey.Id, m.ActiveKey, m.PendingKey)
	}
}

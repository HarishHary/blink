package enrichments

import (
	"context"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

// NewAdapter builds the PluginAdapter for the enrichments plugin type. cfg is
// the config source (snapshot-backed in the data plane, disk-backed in the controller).
func NewAdapter(cfg config.Source[*EnrichmentMetadata]) *plugin.PluginAdapter[Enrichment] {
	return &plugin.PluginAdapter[Enrichment]{
		Key:    "enrichment",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		Config: cfg,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Enrichment, plugin.PluginRPC, error) {
			return plugin.Handshake(ctx, raw, binPath, hash,
				func(fileName string, rpc rpc_enrichments.EnrichmentClient, hash string) Enrichment {
					return newRpcEnrichment(fileName, rpc, cfg, hash)
				})
		},
	}
}

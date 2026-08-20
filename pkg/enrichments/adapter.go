package enrichments

import (
	"context"

	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

// NewAdapter builds the PluginAdapter for the enrichments plugin type.
func NewAdapter() *plugin.Adapter[Enrichment] {
	return &plugin.Adapter[Enrichment]{
		Key:    "enrichment",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		DoHandshake: func(ctx context.Context, raw any, deployment plugin.Deployment) (Enrichment, plugin.RPC, error) {
			metadata, err := (Loader{}).ParseSpec(deployment.Name, deployment.Spec)
			if err != nil {
				return nil, nil, err
			}
			return plugin.Handshake(ctx, raw, deployment,
				func(fileName string, rpc rpc_enrichments.EnrichmentClient, hash string) Enrichment {
					return newRpcEnrichment(fileName, rpc, *metadata, hash)
				})
		},
	}
}

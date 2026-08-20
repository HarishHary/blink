package enrichments

import (
	"context"

	pluginruntime "github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

// NewAdapter builds the PluginAdapter for the enrichments plugin type.
func NewAdapter() *pluginruntime.Adapter[Enrichment] {
	return &pluginruntime.Adapter[Enrichment]{
		Key:    "enrichment",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		DoHandshake: func(ctx context.Context, raw any, deployment pluginruntime.Deployment) (Enrichment, pluginruntime.RPC, error) {
			metadata, err := (Loader{}).ParseSpec(deployment.Name, deployment.Spec)
			if err != nil {
				return nil, nil, err
			}
			return pluginruntime.Handshake(ctx, raw, deployment,
				func(fileName string, rpc rpc_enrichments.EnrichmentClient, hash string) Enrichment {
					return newRpcEnrichment(fileName, rpc, *metadata, hash)
				})
		},
	}
}

package enrichments

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

// NewEnrichmentAdapter builds the PluginAdapter for the enrichments plugin type. cfg is
// the config source (snapshot-backed in the data plane, disk-backed in the controller).
func NewEnrichmentAdapter(cfg config.Source[*EnrichmentMetadata]) *plugin.PluginAdapter[Enrichment] {
	return &plugin.PluginAdapter[Enrichment]{
		Key:    "enrichment",
		Magic:  "enrichment_v1",
		Plugin: &enrichmentPlugin{},
		Config: cfg,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Enrichment, plugin.PluginLifecycle, string, string, error) {
			rpc, ok := raw.(rpc_enrichments.EnrichmentClient)
			if !ok {
				return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
			}

			fileName := helpers.BinaryBaseName(binPath)

			initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := rpc.Init(initCtx, &rpc_enrichments.Empty{})
			cancel()
			if err != nil {
				return nil, nil, "", "", fmt.Errorf("init: %w", err)
			}

			e := newRpcEnrichment(fileName, rpc, cfg, hash)
			id, name := fileName, fileName
			if desired, ok := cfg.DesiredBinaryState(fileName); ok {
				id = desired.Id
				name = desired.Name
			}
			return e, &enrichmentLifecycle{rpc: rpc}, id, name, nil
		},
	}
}

type enrichmentLifecycle struct {
	rpc rpc_enrichments.EnrichmentClient
}

func (l *enrichmentLifecycle) Ping(ctx context.Context) error {
	_, err := l.rpc.Ping(ctx, &rpc_enrichments.Empty{})
	return err
}

func (l *enrichmentLifecycle) Shutdown(ctx context.Context) error {
	_, err := l.rpc.Shutdown(ctx, &rpc_enrichments.Empty{})
	return err
}

type enrichmentPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *enrichmentPlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *enrichmentPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_enrichments.NewEnrichmentClient(c), nil
}

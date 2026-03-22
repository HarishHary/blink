package enrichments

import (
	"context"
	"fmt"
	"time"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

type EnrichmentAdapter struct{}

func (l *EnrichmentAdapter) PluginKey() string         { return "enrichment" }
func (l *EnrichmentAdapter) MagicValue() string        { return "enrichment_v1" }
func (l *EnrichmentAdapter) GRPCPlugin() plugin.Plugin { return &enrichmentPlugin{} }

func (l *EnrichmentAdapter) Handshake(ctx context.Context, raw interface{}, _ string, hash string) (IEnrichment, pluginmgr.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_enrichments.EnrichmentClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	metaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	meta, err := rpc.GetMetadata(metaCtx, &rpc_enrichments.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("metadata: %w", err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = rpc.Init(initCtx, &rpc_enrichments.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	e := newRpcEnrichment(meta, rpc, hash)
	return e, &enrichmentLifecycle{rpc: rpc}, meta.GetId(), meta.GetName(), nil
}

// IsReady always returns true - enrichments have no YAML sidecar prerequisite.
func (l *EnrichmentAdapter) IsReady(_ string) bool                    { return true }
func (l *EnrichmentAdapter) IsShadow(_ string) bool                   { return false }
func (l *EnrichmentAdapter) IsEnabled(_ *pluginmgr.PluginHandle) bool { return true }

func (l *EnrichmentAdapter) Workers(_ string) int { return 1 }

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

type enrichmentPlugin struct{ plugin.NetRPCUnsupportedPlugin }

func (p *enrichmentPlugin) GRPCServer(_ *plugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *enrichmentPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_enrichments.NewEnrichmentClient(c), nil
}

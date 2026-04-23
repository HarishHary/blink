package enrichments

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/enrichments/rpc_enrichments"
)

type EnrichmentConfigAdapter struct {
	Manager *EnrichmentConfigManager
}

func (l *EnrichmentConfigAdapter) PluginKey() string           { return "enrichment" }
func (l *EnrichmentConfigAdapter) MagicValue() string          { return "enrichment_v1" }
func (l *EnrichmentConfigAdapter) GRPCPlugin() goplugin.Plugin { return &enrichmentPlugin{} }

func (l *EnrichmentConfigAdapter) Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (Enrichment, plugin.PluginLifecycle, string, string, error) {
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

	e := newRpcEnrichment(fileName, rpc, l.Manager, hash)
	cfg, ok := l.Manager.Current().ByFileName(fileName)
	id, name := fileName, fileName
	if ok {
		id = cfg.Id
		name = cfg.Name
	}
	return e, &enrichmentLifecycle{rpc: rpc}, id, name, nil
}

// IsReady reports whether this binary's YAML sidecar exists in the current registry.
func (l *EnrichmentConfigAdapter) IsReady(binPath string) bool {
	_, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(binPath))
	return ok
}

// IsShadow reports whether this binary's YAML declares it as a shadow or canary version.
func (l *EnrichmentConfigAdapter) IsShadow(binPath string) bool {
	cfg, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok {
		return false
	}
	m := cfg.RolloutMode
	return m == internal.RolloutModeCanary || m == internal.RolloutModeShadow
}

// IsEnabled reports whether the enrichment's YAML sidecar still exists and is enabled.
func (l *EnrichmentConfigAdapter) IsEnabled(h *plugin.PluginHandle) bool {
	cfg, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(h.BinPath))
	return ok && cfg.Enabled
}

func (l *EnrichmentConfigAdapter) Workers(binPath string) int {
	cfg, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok || cfg.MaxProcs <= 0 {
		return 1
	}
	return cfg.MaxProcs
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

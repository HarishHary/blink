package matchers

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/matchers/config"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

type MatcherAdapter struct {
	Watcher *config.Watcher
}

func (l *MatcherAdapter) PluginKey() string         { return "matcher" }
func (l *MatcherAdapter) MagicValue() string        { return "matcher_v1" }
func (l *MatcherAdapter) GRPCPlugin() goplugin.Plugin { return &matcherPlugin{} }

// Handshake connects to the matcher subprocess, calls Init, and returns a
// ready rpcMatcher. Identity comes from the YAML sidecar, not from a GetMetadata RPC.
func (l *MatcherAdapter) Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (Matcher, plugin.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_matchers.MatcherClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	fileName := helpers.BinaryBaseName(binPath)

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := rpc.Init(initCtx, &rpc_matchers.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	m := newRpcMatcher(fileName, rpc, l.Watcher, 5*time.Second, hash)
	cfg, ok := l.Watcher.Current().ByFileName(fileName)
	id, name := fileName, fileName
	if ok {
		id = cfg.Id
		name = cfg.Name
	}
	return m, &matcherLifecycle{rpc: rpc}, id, name, nil
}

// IsReady reports whether this binary's YAML sidecar exists in the current registry.
func (l *MatcherAdapter) IsReady(binPath string) bool {
	_, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(binPath))
	return ok
}

// IsShadow reports whether this binary's YAML declares it as a shadow or canary version.
func (l *MatcherAdapter) IsShadow(binPath string) bool {
	cfg, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok {
		return false
	}
	m := cfg.RolloutMode
	return m == internal.RolloutModeCanary || m == internal.RolloutModeShadow
}

// IsEnabled reports whether the matcher's YAML sidecar still exists and is enabled.
func (l *MatcherAdapter) IsEnabled(h *plugin.PluginHandle) bool {
	cfg, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(h.BinPath))
	return ok && cfg.Enabled
}

func (l *MatcherAdapter) Workers(binPath string) int {
	cfg, ok := l.Watcher.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok || cfg.MaxProcs <= 0 {
		return 1
	}
	return cfg.MaxProcs
}

type matcherLifecycle struct{ rpc rpc_matchers.MatcherClient }

func (l *matcherLifecycle) Ping(ctx context.Context) error {
	_, err := l.rpc.Ping(ctx, &rpc_matchers.Empty{})
	return err
}

func (l *matcherLifecycle) Shutdown(ctx context.Context) error {
	_, err := l.rpc.Shutdown(ctx, &rpc_matchers.Empty{})
	return err
}

type matcherPlugin struct{ goplugin.NetRPCUnsupportedPlugin }

func (p *matcherPlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *matcherPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_matchers.NewMatcherClient(c), nil
}

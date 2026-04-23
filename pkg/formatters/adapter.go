package formatters

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

// FormatterAdapter implements goplugin.PluginAdapter[Formatter].
type FormatterAdapter struct {
	Manager *FormatterConfigManager
}

func (l *FormatterAdapter) PluginKey() string           { return "formatter" }
func (l *FormatterAdapter) MagicValue() string          { return "formatter_v1" }
func (l *FormatterAdapter) GRPCPlugin() goplugin.Plugin { return &formatterPlugin{} }

// Handshake connects to the formatter subprocess, calls Init, and returns a
// ready rpcFormatter. Identity comes from the YAML sidecar, not from a GetMetadata RPC.
func (l *FormatterAdapter) Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (Formatter, plugin.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_formatters.FormatterClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	fileName := helpers.BinaryBaseName(binPath)

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := rpc.Init(initCtx, &rpc_formatters.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	f := newRpcFormatter(fileName, rpc, l.Manager, hash)
	cfg, ok := l.Manager.Current().ByFileName(fileName)
	id, name := fileName, fileName
	if ok {
		id = cfg.Id
		name = cfg.Name
	}
	return f, &formatterLifecycle{rpc: rpc}, id, name, nil
}

// IsReady reports whether this binary's YAML sidecar exists in the current registry.
func (l *FormatterAdapter) IsReady(binPath string) bool {
	_, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(binPath))
	return ok
}

// IsShadow reports whether this binary's YAML declares it as a shadow or canary version.
func (l *FormatterAdapter) IsShadow(binPath string) bool {
	cfg, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok {
		return false
	}
	m := cfg.RolloutMode
	return m == internal.RolloutModeCanary || m == internal.RolloutModeShadow
}

// IsEnabled reports whether the formatter's YAML sidecar still exists and is enabled.
func (l *FormatterAdapter) IsEnabled(h *plugin.PluginHandle) bool {
	cfg, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(h.BinPath))
	return ok && cfg.Enabled
}

func (l *FormatterAdapter) Workers(binPath string) int {
	cfg, ok := l.Manager.Current().ByFileName(helpers.BinaryBaseName(binPath))
	if !ok || cfg.MaxProcs <= 0 {
		return 1
	}
	return cfg.MaxProcs
}

type formatterLifecycle struct {
	rpc rpc_formatters.FormatterClient
}

func (l *formatterLifecycle) Ping(ctx context.Context) error {
	_, err := l.rpc.Ping(ctx, &rpc_formatters.Empty{})
	return err
}

func (l *formatterLifecycle) Shutdown(ctx context.Context) error {
	_, err := l.rpc.Shutdown(ctx, &rpc_formatters.Empty{})
	return err
}

type formatterPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *formatterPlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *formatterPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_formatters.NewFormatterClient(c), nil
}

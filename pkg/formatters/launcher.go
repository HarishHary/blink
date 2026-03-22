package formatters

import (
	"context"
	"fmt"
	"time"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

// FormatterAdapter implements pluginmgr.PluginAdapter[IFormatter].
type FormatterAdapter struct{}

func (l *FormatterAdapter) PluginKey() string         { return "formatter" }
func (l *FormatterAdapter) MagicValue() string        { return "formatter_v1" }
func (l *FormatterAdapter) GRPCPlugin() plugin.Plugin { return &formatterPlugin{} }

func (l *FormatterAdapter) Handshake(ctx context.Context, raw interface{}, _ string, hash string) (IFormatter, pluginmgr.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_formatters.FormatterClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	metaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	meta, err := rpc.GetMetadata(metaCtx, &rpc_formatters.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("metadata: %w", err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = rpc.Init(initCtx, &rpc_formatters.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	f := newRpcFormatter(meta, rpc, hash)
	return f, &formatterLifecycle{rpc: rpc}, meta.GetId(), meta.GetName(), nil
}

// IsReady always returns true - formatters have no YAML sidecar prerequisite.
func (l *FormatterAdapter) IsReady(_ string) bool                    { return true }
func (l *FormatterAdapter) IsShadow(_ string) bool                   { return false }
func (l *FormatterAdapter) IsEnabled(_ *pluginmgr.PluginHandle) bool { return true }

func (l *FormatterAdapter) Workers(_ string) int { return 1 }

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

type formatterPlugin struct{ plugin.NetRPCUnsupportedPlugin }

func (p *formatterPlugin) GRPCServer(_ *plugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *formatterPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_formatters.NewFormatterClient(c), nil
}

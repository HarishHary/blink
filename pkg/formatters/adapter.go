package formatters

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

// NewFormatterAdapter builds the PluginAdapter for the formatters plugin type.
func NewFormatterAdapter(manager *FormatterConfigManager) *plugin.PluginAdapter[Formatter] {
	return &plugin.PluginAdapter[Formatter]{
		Key:           "formatter",
		Magic:         "formatter_v1",
		Plugin:        &formatterPlugin{},
		DesiredConfig: manager,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Formatter, plugin.PluginLifecycle, string, string, error) {
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

			f := newRpcFormatter(fileName, rpc, manager, hash)
			id, name := fileName, fileName
			if desired, ok := manager.DesiredBinaryState(fileName); ok {
				id = desired.ID
				name = desired.Name
			}
			return f, &formatterLifecycle{rpc: rpc}, id, name, nil
		},
	}
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

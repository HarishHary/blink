package matchers

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

// NewMatcherAdapter builds the PluginAdapter for the matchers plugin type.
func NewMatcherAdapter(manager *MatcherConfigWatcher) *plugin.PluginAdapter[Matcher] {
	return &plugin.PluginAdapter[Matcher]{
		Key:           "matcher",
		Magic:         "matcher_v1",
		Plugin:        &matcherPlugin{},
		DesiredConfig: manager,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Matcher, plugin.PluginLifecycle, string, string, error) {
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

			m := newRpcMatcher(fileName, rpc, manager, 5*time.Second, hash)
			id, name := fileName, fileName
			if desired, ok := manager.DesiredBinaryState(fileName); ok {
				id = desired.ID
				name = desired.Name
			}
			return m, &matcherLifecycle{rpc: rpc}, id, name, nil
		},
	}
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

type matcherPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *matcherPlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *matcherPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_matchers.NewMatcherClient(c), nil
}

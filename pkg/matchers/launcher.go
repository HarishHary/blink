package matchers

import (
	"context"
	"fmt"
	"time"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

type MatcherAdapter struct{}

func (l *MatcherAdapter) PluginKey() string         { return "matcher" }
func (l *MatcherAdapter) MagicValue() string        { return "matcher_v1" }
func (l *MatcherAdapter) GRPCPlugin() plugin.Plugin { return &matcherPlugin{} }

func (l *MatcherAdapter) Handshake(ctx context.Context, raw interface{}, _ string, hash string) (Matcher, pluginmgr.PluginLifecycle, string, string, error) {
	rpc, ok := raw.(rpc_matchers.MatcherClient)
	if !ok {
		return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
	}

	metaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	meta, err := rpc.GetMetadata(metaCtx, &rpc_matchers.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("metadata: %w", err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = rpc.Init(initCtx, &rpc_matchers.Empty{})
	cancel()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("init: %w", err)
	}

	m := newRpcMatcher(meta, rpc, 5*time.Second, hash)
	return m, &matcherLifecycle{rpc: rpc}, meta.GetId(), meta.GetName(), nil
}

func (l *MatcherAdapter) IsEnabled(_ *pluginmgr.PluginHandle) bool { return true }

func (l *MatcherAdapter) Workers(_ string) int { return 1 }

type matcherLifecycle struct{ rpc rpc_matchers.MatcherClient }

func (l *matcherLifecycle) Ping(ctx context.Context) error {
	_, err := l.rpc.Ping(ctx, &rpc_matchers.Empty{})
	return err
}

func (l *matcherLifecycle) Shutdown(ctx context.Context) error {
	_, err := l.rpc.Shutdown(ctx, &rpc_matchers.Empty{})
	return err
}

type matcherPlugin struct{ plugin.NetRPCUnsupportedPlugin }

func (p *matcherPlugin) GRPCServer(_ *plugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *matcherPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_matchers.NewMatcherClient(c), nil
}

package rules

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

// NewAdapter builds the PluginAdapter for the rules plugin type.
func NewAdapter(cfg config.Source[*RuleMetadata]) *plugin.PluginAdapter[Rule] {
	return &plugin.PluginAdapter[Rule]{
		Key:    "rule",
		Magic:  MagicValue,
		Plugin: &rulePlugin{},
		Config: cfg,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Rule, plugin.PluginLifecycle, string, string, error) {
			rpc, ok := raw.(rpc_rules.RuleClient)
			if !ok {
				return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
			}

			desired, ok := cfg.DesiredBinaryState(helpers.BinaryBaseName(binPath))
			if !ok {
				return nil, nil, "", "", fmt.Errorf("rule launcher: no snapshot spec for binary %q", binPath)
			}

			initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := rpc.Init(initCtx, &rpc_rules.Empty{})
			cancel()
			if err != nil {
				return nil, nil, "", "", fmt.Errorf("init: %w", err)
			}

			rule := newRpcRule(helpers.BinaryBaseName(binPath), rpc, cfg, hash)
			return rule, &ruleLifecycle{rpc: rpc}, desired.Id, desired.Name, nil
		},
	}
}

type ruleLifecycle struct {
	rpc rpc_rules.RuleClient
}

func (l *ruleLifecycle) Ping(ctx context.Context) error {
	_, err := l.rpc.Ping(ctx, &rpc_rules.Empty{})
	return err
}

func (l *ruleLifecycle) Shutdown(ctx context.Context) error {
	_, err := l.rpc.Shutdown(ctx, &rpc_rules.Empty{})
	return err
}

type rulePlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *rulePlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error { return nil }
func (p *rulePlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_rules.NewRuleClient(c), nil
}

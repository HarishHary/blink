package rules

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

// NewRuleAdapter builds the PluginAdapter for the rules plugin type.
// manager is used both as the PluginController and for rpcRule construction.
func NewRuleAdapter(manager *RuleConfigManager) *plugin.PluginAdapter[Rule] {
	return &plugin.PluginAdapter[Rule]{
		Key:        "rule",
		Magic:      "rule_v1",
		Plugin:     &rulePlugin{},
		Controller: manager,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Rule, plugin.PluginLifecycle, string, string, error) {
			rpc, ok := raw.(rpc_rules.RuleClient)
			if !ok {
				return nil, nil, "", "", fmt.Errorf("dispense: unexpected type %T", raw)
			}

			desired, ok := manager.DesiredForBinary(helpers.BinaryBaseName(binPath))
			if !ok {
				return nil, nil, "", "", fmt.Errorf("rule launcher: no YAML sidecar found for binary %q", binPath)
			}

			initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := rpc.Init(initCtx, &rpc_rules.Empty{})
			cancel()
			if err != nil {
				return nil, nil, "", "", fmt.Errorf("init: %w", err)
			}

			rule := newRpcRule(helpers.BinaryBaseName(binPath), rpc, manager, hash)
			return rule, &ruleLifecycle{rpc: rpc}, desired.ID, desired.Name, nil
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

// rulePlugin is the go-plugin client-side stub.
type rulePlugin struct {
	goplugin.NetRPCUnsupportedPlugin
}

func (p *rulePlugin) GRPCServer(_ *goplugin.GRPCBroker, _ *grpc.Server) error {
	return nil
}
func (p *rulePlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return rpc_rules.NewRuleClient(c), nil
}

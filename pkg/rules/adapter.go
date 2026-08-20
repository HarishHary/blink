package rules

import (
	"context"

	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

// NewAdapter builds the PluginAdapter for the rules plugin type.
func NewAdapter() *plugin.Adapter[Rule] {
	return &plugin.Adapter[Rule]{
		Key:    "rule",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		DoHandshake: func(ctx context.Context, raw any, deployment plugin.Deployment) (Rule, plugin.RPC, error) {
			metadata, err := (Loader{}).ParseSpec(deployment.Name, deployment.Spec)
			if err != nil {
				return nil, nil, err
			}
			return plugin.Handshake(ctx, raw, deployment,
				func(fileName string, rpc rpc_rules.RuleClient, hash string) Rule {
					return newRpcRule(fileName, rpc, *metadata, hash)
				})
		},
	}
}

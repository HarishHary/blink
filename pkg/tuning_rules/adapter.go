package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

// NewAdapter builds the PluginAdapter for the tuning_rules plugin type.
func NewAdapter() *plugin.Adapter[TuningRule] {
	return &plugin.Adapter[TuningRule]{
		Key:    "tuning_rule",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		DoHandshake: func(ctx context.Context, raw any, deployment plugin.Deployment) (TuningRule, plugin.RPC, error) {
			metadata, err := (Loader{}).ParseSpec(deployment.Name, deployment.Spec)
			if err != nil {
				return nil, nil, err
			}
			return plugin.Handshake(ctx, raw, deployment,
				func(fileName string, rpc rpc_tuning_rules.TuningRuleClient, hash string) TuningRule {
					return newRpcTuningRule(fileName, rpc, *metadata, hash)
				})
		},
	}
}

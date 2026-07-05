package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/tuning_rules/rpc_tuning_rules"
)

// NewAdapter builds the PluginAdapter for the tuning_rules plugin type. cfg is
// the config source (snapshot-backed in the data plane, disk-backed in the controller).
func NewAdapter(cfg config.Source[*TuningRuleMetadata]) *plugin.PluginAdapter[TuningRule] {
	return &plugin.PluginAdapter[TuningRule]{
		Key:    "tuning_rule",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		Config: cfg,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (TuningRule, plugin.PluginRPC, error) {
			return plugin.Handshake(ctx, raw, binPath, hash,
				func(fileName string, rpc rpc_tuning_rules.TuningRuleClient, hash string) TuningRule {
					return newRpcTuningRule(fileName, rpc, cfg, hash)
				})
		},
	}
}

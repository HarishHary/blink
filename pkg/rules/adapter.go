package rules

import (
	"context"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/rules/rpc_rules"
)

// NewAdapter builds the PluginAdapter for the rules plugin type.
func NewAdapter(cfg config.Source[*RuleMetadata]) *plugin.PluginAdapter[Rule] {
	return &plugin.PluginAdapter[Rule]{
		Key:    "rule",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		Config: cfg,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Rule, plugin.PluginRPC, error) {
			return plugin.Handshake(ctx, raw, binPath, hash,
				func(fileName string, rpc rpc_rules.RuleClient, hash string) Rule {
					return newRpcRule(fileName, rpc, cfg, hash)
				})
		},
	}
}

package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

// NewAdapter builds the PluginAdapter for the formatters plugin type.
func NewAdapter() *plugin.Adapter[Formatter] {
	return &plugin.Adapter[Formatter]{
		Key:    "formatter",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		DoHandshake: func(ctx context.Context, raw any, deployment plugin.Deployment) (Formatter, plugin.RPC, error) {
			metadata, err := (Loader{}).ParseSpec(deployment.Name, deployment.Spec)
			if err != nil {
				return nil, nil, err
			}
			return plugin.Handshake(ctx, raw, deployment,
				func(fileName string, rpc rpc_formatters.FormatterClient, hash string) Formatter {
					return newRpcFormatter(fileName, rpc, *metadata, hash)
				})
		},
	}
}

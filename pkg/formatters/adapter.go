package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/formatters/rpc_formatters"
)

func NewAdapter(cfg config.Source[*FormatterMetadata]) *plugin.PluginAdapter[Formatter] {
	return &plugin.PluginAdapter[Formatter]{
		Key:    "formatter",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		Config: cfg,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Formatter, plugin.PluginRPC, error) {
			return plugin.Handshake(ctx, raw, binPath, hash,
				func(fileName string, rpc rpc_formatters.FormatterClient, hash string) Formatter {
					return newRpcFormatter(fileName, rpc, cfg, hash)
				})
		},
	}
}

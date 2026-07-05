package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

// NewAdapter builds the PluginAdapter for the matchers plugin type. cfg is the
// config source (snapshot-backed in the data plane, disk-backed in the controller).
func NewAdapter(cfg config.Source[*MatcherMetadata]) *plugin.PluginAdapter[Matcher] {
	return &plugin.PluginAdapter[Matcher]{
		Key:    "matcher",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		Config: cfg,
		DoHandshake: func(ctx context.Context, raw any, binPath, hash string) (Matcher, plugin.PluginRPC, error) {
			return plugin.Handshake(ctx, raw, binPath, hash,
				func(fileName string, rpc rpc_matchers.MatcherClient, hash string) Matcher {
					return newRpcMatcher(fileName, rpc, cfg, hash)
				})
		},
	}
}

package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/matchers/rpc_matchers"
)

// NewAdapter builds the PluginAdapter for the matchers plugin type.
func NewAdapter() *plugin.Adapter[Matcher] {
	return &plugin.Adapter[Matcher]{
		Key:    "matcher",
		Magic:  MagicValue,
		Plugin: &pluginImpl{},
		DoHandshake: func(ctx context.Context, raw any, deployment plugin.Deployment) (Matcher, plugin.RPC, error) {
			metadata, err := (Loader{}).ParseSpec(deployment.Name, deployment.Spec)
			if err != nil {
				return nil, nil, err
			}
			return plugin.Handshake(ctx, raw, deployment,
				func(fileName string, rpc rpc_matchers.MatcherClient, hash string) Matcher {
					return newRpcMatcher(fileName, rpc, *metadata, hash)
				})
		},
	}
}

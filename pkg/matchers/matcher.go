package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/config"
)

// PluginMetadata is re-exported from internal/plugin so plugin authors don't need to
// import an internal package.
type PluginMetadata = plugin.PluginMetadata

type Matcher interface {
	MatcherMetadata() *config.MatcherMetadata
	PluginMetadata() plugin.PluginMetadata // satisfies plugin.Syncable
	Global() bool
	Checksum() string
	String() string
	Match(ctx context.Context, evts []events.Event) ([]bool, errors.Error)
}

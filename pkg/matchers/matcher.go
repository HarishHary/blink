package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/config"
)

type PluginMetadata = plugin.PluginMetadata
type MatcherMetadata = config.MatcherMetadata

type Matcher interface {
	MatcherMetadata() *MatcherMetadata
	Metadata() PluginMetadata
	Global() bool
	Checksum() string
	String() string
	Match(ctx context.Context, evts []events.Event) ([]bool, errors.Error)
}

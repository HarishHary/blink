package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
)

type PluginMetadata = plugin.PluginMetadata

// MatcherMetadata is the in-memory representation of a matcher YAML sidecar.
type MatcherMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	Global                bool `yaml:"global"`
}

type Matcher interface {
	MatcherMetadata() *MatcherMetadata
	Metadata() PluginMetadata
	Global() bool
	Checksum() string
	String() string
	Match(ctx context.Context, evts []events.Event) ([]bool, errors.Error)
}

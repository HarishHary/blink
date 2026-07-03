package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
)

// MatcherMetadata is the in-memory representation of a matcher YAML sidecar.
type MatcherMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	Global                bool `yaml:"global"`
}

type Matcher interface {
	Match(ctx context.Context, evts []events.Event) ([]bool, errors.Error)

	MatcherMetadata() *MatcherMetadata
	Metadata() plugin.PluginMetadata
	Checksum() string
	String() string
}

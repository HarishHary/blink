package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
)

type PluginMetadata = plugin.PluginMetadata

// FormatterMetadata is the in-memory representation of a formatter YAML sidecar.
type FormatterMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
}

type Formatter interface {
	Format(ctx context.Context, alerts []*alerts.Alert) ([]map[string]any, errors.Error)

	FormatterMetadata() *FormatterMetadata
	Metadata() PluginMetadata
	Checksum() string
	String() string
}

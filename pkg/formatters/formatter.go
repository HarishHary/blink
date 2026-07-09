package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
)

// FormatterMetadata is the in-memory representation of a formatter YAML sidecar.
type FormatterMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
}

// Formatter formats alert batches and exposes its live sidecar metadata.
type Formatter interface {
	plugin.Syncable
	Format(ctx context.Context, alerts []*alerts.Alert) ([]map[string]any, errors.Error)
	FormatterMetadata() *FormatterMetadata
}

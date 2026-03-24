package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters/config"
)

type PluginMetadata = plugin.PluginMetadata
type FormatterMetadata = config.FormatterMetadata

type Formatter interface {
	Format(ctx context.Context, alerts []*alerts.Alert) ([]map[string]any, errors.Error)

	FormatterMetadata() *FormatterMetadata
	Metadata() PluginMetadata
	Checksum() string
	String() string
}

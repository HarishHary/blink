package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters/config"
)

// PluginMetadata is re-exported from internal/plugin so plugin authors don't need to
// import an internal package.
type PluginMetadata = plugin.PluginMetadata

type Formatter interface {
	Format(ctx context.Context, alerts []*alerts.Alert) ([]map[string]any, errors.Error)

	FormatterMetadata() *config.FormatterMetadata
	PluginMetadata() plugin.PluginMetadata // satisfies plugin.Syncable
	Checksum() string
	String() string
}

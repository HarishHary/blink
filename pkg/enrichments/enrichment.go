package enrichments

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments/config"
)

// PluginMetadata is re-exported from internal/plugin so plugin authors don't need to
// import an internal package.
type PluginMetadata = plugin.PluginMetadata
type EnrichmentMetadata = config.EnrichmentMetadata

type Enrichment interface {
	Enrich(ctx context.Context, alerts []*alerts.Alert) errors.Error
	// DependsOn returns plugin names that must run before this enrichment.
	// Populated from the YAML sidecar depends_on field.
	DependsOn() []string

	EnrichmentMetadata() *EnrichmentMetadata
	Metadata() PluginMetadata // satisfies plugin.Syncable
	Checksum() string
	String() string
}

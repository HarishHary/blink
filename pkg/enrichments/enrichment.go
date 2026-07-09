package enrichments

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
)

// EnrichmentMetadata is the in-memory representation of an enrichment YAML sidecar.
type EnrichmentMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	DependsOn             []string `yaml:"depends_on"`
}

// Enrichment mutates alert batches and exposes its live sidecar metadata.
type Enrichment interface {
	plugin.Syncable
	Enrich(ctx context.Context, alerts []*alerts.Alert) errors.Error
	EnrichmentMetadata() *EnrichmentMetadata
}

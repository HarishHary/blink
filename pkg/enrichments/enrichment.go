package enrichments

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
)

type EnrichmentMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	DependsOn             []string `yaml:"depends_on"`
}

type Enrichment interface {
	plugin.Syncable
	Enrich(ctx context.Context, alerts []*alerts.Alert) errors.Error
	EnrichmentMetadata() *EnrichmentMetadata
}

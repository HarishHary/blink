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
	Enrich(ctx context.Context, alerts []*alerts.Alert) errors.Error
	DependsOn() []string

	EnrichmentMetadata() *EnrichmentMetadata
	Metadata() plugin.PluginMetadata
	Checksum() string
	String() string
}

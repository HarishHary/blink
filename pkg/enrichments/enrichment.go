package enrichments

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/alerts"
)

// EnrichmentMetadata is the in-memory representation of an enrichment YAML sidecar.
type EnrichmentMetadata struct {
	plugin.Spec `yaml:",inline"`
	DependsOn   []string `yaml:"depends_on"`
}

// Clone returns an independently owned copy safe to pass across actor boundaries.
func (m *EnrichmentMetadata) Clone() *EnrichmentMetadata {
	if m == nil {
		return nil
	}
	clone := *m
	clone.DependsOn = append([]string(nil), m.DependsOn...)
	return &clone
}

// Enrichment mutates alert batches and exposes its live sidecar metadata.
type Enrichment interface {
	plugin.Artifact
	EnrichBatch(ctx context.Context, batch *alerts.Batch) EnrichResult
	EnrichmentMetadata() *EnrichmentMetadata
}

// EnrichResult holds the aligned per-alert and whole-call outcomes.
type EnrichResult struct {
	Errs    []errors.Error
	CallErr errors.Error
}

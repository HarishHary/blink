package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/alerts"
)

// FormatterMetadata is the in-memory representation of a formatter YAML sidecar.
type FormatterMetadata struct {
	plugin.Spec `yaml:",inline"`
}

// Clone returns an independently owned copy safe to pass across actor boundaries.
func (m *FormatterMetadata) Clone() *FormatterMetadata {
	if m == nil {
		return nil
	}
	clone := *m
	return &clone
}

// Formatter formats alert batches and exposes its live sidecar metadata.
type Formatter interface {
	plugin.Artifact
	FormatBatch(ctx context.Context, batch *alerts.Batch) FormatResult
	FormatterMetadata() *FormatterMetadata
}

// FormatItem holds one alert's formatting outcome.
type FormatItem struct {
	Output map[string]any
	Err    errors.Error
}

// FormatResult holds a batch-level formatter result.
type FormatResult struct {
	Items   []FormatItem
	CallErr errors.Error // whole-call failure; never alert-scoped
}

package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	evts "github.com/harishhary/blink/pkg/events"
)

// MatcherMetadata is the in-memory representation of a matcher YAML sidecar.
type MatcherMetadata struct {
	plugin.Spec `yaml:",inline"`
	Global      bool `yaml:"global"`
}

// Clone returns an independently owned copy safe to pass across actor boundaries.
func (m *MatcherMetadata) Clone() *MatcherMetadata {
	if m == nil {
		return nil
	}
	clone := *m
	return &clone
}

// Matcher matches batches of events and exposes its live sidecar metadata.
type Matcher interface {
	plugin.Artifact
	MatchBatch(ctx context.Context, events []evts.Event) MatchResult
	MatcherMetadata() *MatcherMetadata
}

// MatchItem holds one event's match outcome.
type MatchItem struct {
	Matched bool
	Err     errors.Error
}

// MatchResult holds the result from matching events.
type MatchResult struct {
	Items   []MatchItem
	CallErr errors.Error
}

package rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/config"
	"github.com/harishhary/blink/pkg/scoring"
)

type Observables = config.Observable

// Metadata carries all static rule configuration. Alert.Rule is typed as Metadata so downstream pipeline services (tuner, enricher, formatter, dispatcher) can read rule properties without needing an Evaluate capability.
type Metadata interface {
	Id() string
	Name() string
	Description() string
	Enabled() bool
	FileName() string
	DisplayName() string
	References() []string
	Severity() scoring.Severity
	Confidence() scoring.Confidence
	RiskScore() scoring.RiskScore
	MergeByKeys() []string
	MergeWindowMins() time.Duration
	ReqSubkeys() []string
	Signal() bool
	SignalThreshold() scoring.Confidence
	Tags() []string
	Dispatchers() []string
	LogTypes() []string
	Observables() []Observables
	Matchers() []string
	Formatters() []string
	Enrichments() []string
	TuningRules() []string
	Checksum() string
	Version() string
}

// Rule is the full interface for live rule plugins: metadata + batch evaluation.
// All rules receive a slice of events and return a matched bool per event.
// The SDK server handles looping over individual events on the subprocess side.
type Rule interface {
	Metadata
	Evaluate(ctx context.Context, evts []events.Event) ([]bool, errors.Error)
}

// --- Optional capability interfaces ---
// Discovered via type assertion; not required by all implementations.

// Generates a dynamic alert title from the triggering event.
type Titler interface {
	AlertTitle(event events.Event) string
}

// Generates a dynamic alert description from the triggering event.
type Describer interface {
	AlertDescription(event events.Event) string
}

// Returns keys used to deduplicate/merge related alerts.
type Deduper interface {
	Dedup(event events.Event) []string
}

// Computes a per-event severity (e.g. based on asset value).
type DynamicSeverity interface {
	DynamicSeverity(event events.Event) scoring.Severity
}

// Appends extra key-value context to the generated alert.
type ContextProvider interface {
	AlertContext(event events.Event) map[string]any
}

// Guards rule evaluation until required event fields are present.
type SubKeyFilter interface{ SubKeysInEvent(event events.Event) bool }

package rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/scoring"
)

// EventResult is the per-event outcome returned by Rule.Evaluate.
// Fields beyond Matched are populated only when the plugin implements the
// corresponding optional capability interface (Titler, Describer, etc.).
// An empty/zero field means "use the YAML-configured default".
type EventResult struct {
	Matched     bool
	Title       string
	Description string
	Severity    string         // "" = no override; "info"/"low"/"medium"/"high"/"critical" = override
	Context     map[string]any // extra key-value pairs merged into alert.Event
	MergeByKeys []string       // overrides YAML merge_by_keys when non-nil
}

// Observable describes one observable field that a rule can surface in an alert.
type Observable struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Aggregation bool   `yaml:"aggregation"`
}

// RuleMetadata is the in-memory representation of a rule YAML sidecar file.
type RuleMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`

	// Scoring
	SeverityStr        string `yaml:"severity"`
	ConfidenceStr      string `yaml:"confidence"`
	SignalThresholdStr string `yaml:"signal_threshold"`

	// Rollout / matching
	LogTypes   []string `yaml:"log_types"`
	Matchers   []string `yaml:"matchers"`
	ReqSubkeys []string `yaml:"req_subkeys"`

	// Merging
	MergeByKeys          []string `yaml:"merge_by_keys"`
	MergeWindowMinsField uint32   `yaml:"merge_window_mins"`

	// Signal
	Signal bool `yaml:"signal"`

	// Labelling
	Tags       []string `yaml:"tags"`
	References []string `yaml:"references"`

	// Observables - static fields the rule surfaces in generated alerts.
	Observables []Observable `yaml:"observables"`

	// Pipeline stages
	Dispatchers []string `yaml:"dispatchers"`
	Formatters  []string `yaml:"formatters"`
	Enrichments []string `yaml:"enrichments"`
	TuningRules []string `yaml:"tuning_rules"`

	// Parsed scoring values - populated by resolveScoring(); yaml:"-" so they aren't (re)serialized.
	Severity        scoring.Severity   `yaml:"-"`
	Confidence      scoring.Confidence `yaml:"-"`
	SignalThreshold scoring.Confidence `yaml:"-"`
	RiskScore       scoring.RiskScore  `yaml:"-"`
}

// Load reads and validates a single YAML sidecar file, returning a *RuleMetadata

// New constructs a RuleMetadata from already-parsed field values (e.g. from a proto payload).
func NewRuleMetadata(c RuleMetadata) (*RuleMetadata, error) {
	if err := c.resolveScoring(); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolveScoring parses the string scoring fields to their typed equivalents
// and computes the risk score.
func (c *RuleMetadata) resolveScoring() error {
	var err error
	if c.SeverityStr != "" {
		c.Severity, err = scoring.ParseSeverity(c.SeverityStr)
		if err != nil {
			return err
		}
	}
	if c.ConfidenceStr != "" {
		c.Confidence, err = scoring.ParseConfidence(c.ConfidenceStr)
		if err != nil {
			return err
		}
	}
	if c.SignalThresholdStr != "" {
		c.SignalThreshold, err = scoring.ParseConfidence(c.SignalThresholdStr)
		if err != nil {
			return err
		}
	}
	c.RiskScore = scoring.ComputeRiskScore(c.Confidence, c.Severity)
	return nil
}

func (c *RuleMetadata) MergeWindowMins() time.Duration {
	return time.Duration(c.MergeWindowMinsField) * time.Minute
}

// Rule is the full interface for live rule plugins: config accessor + batch evaluation.
// All rules receive a slice of events and return one EvalResult per event.
type Rule interface {
	Evaluate(ctx context.Context, evts []events.Event) ([]EventResult, errors.Error)

	plugin.Syncable
	RuleMetadata() *RuleMetadata
}

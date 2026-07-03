package rules

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/scoring"
)

// EvalResult is the per-event outcome returned by Rule.Evaluate.
// Fields beyond Matched are populated only when the plugin implements the
// corresponding optional capability interface (Titler, Describer, etc.).
// An empty/zero field means "use the YAML-configured default".
type EvalResult struct {
	Matched     bool
	Title       string
	Description string
	Severity    string         // "" = no override; "info"/"low"/"medium"/"high"/"critical" = override
	Context     map[string]any // extra key-value pairs merged into alert.Event
	MergeByKeys []string       // overrides YAML merge_by_keys when non-nil
}

// Observable describes one observable field that a rule can surface in an alert.
type Observable struct {
	NameVal        string `yaml:"name"`
	DescriptionVal string `yaml:"description"`
	AggregationVal bool   `yaml:"aggregation"`
}

func (o *Observable) Name() string        { return o.NameVal }
func (o *Observable) Description() string { return o.DescriptionVal }
func (o *Observable) Aggregation() bool   { return o.AggregationVal }

// RuleMetadata is the in-memory representation of a rule YAML sidecar file.
type RuleMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`

	// Scoring
	SeverityStr        string `yaml:"severity"`
	ConfidenceStr      string `yaml:"confidence"`
	SignalThresholdStr string `yaml:"signal_threshold"`

	// Routing / matching
	LogTypesField   []string `yaml:"log_types"`
	MatchersField   []string `yaml:"matchers"`
	ReqSubkeysField []string `yaml:"req_subkeys"`

	// Merging
	MergeByKeysField     []string `yaml:"merge_by_keys"`
	MergeWindowMinsField uint32   `yaml:"merge_window_mins"`

	// Signal
	SignalField bool `yaml:"signal"`

	// Labelling
	TagsField       []string `yaml:"tags"`
	ReferencesField []string `yaml:"references"`

	// Observables - static fields the rule surfaces in generated alerts.
	ObservablesField []Observable `yaml:"observables"`

	// Pipeline stages
	DispatchersField []string `yaml:"dispatchers"`
	FormattersField  []string `yaml:"formatters"`
	EnrichmentsField []string `yaml:"enrichments"`
	TuningRulesField []string `yaml:"tuning_rules"`

	// Parsed scoring values - populated by Load(); not read from YAML directly.
	severity        scoring.Severity
	confidence      scoring.Confidence
	signalThreshold scoring.Confidence
	riskScore       scoring.RiskScore
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
		c.severity, err = scoring.ParseSeverity(c.SeverityStr)
		if err != nil {
			return err
		}
	}
	if c.ConfidenceStr != "" {
		c.confidence, err = scoring.ParseConfidence(c.ConfidenceStr)
		if err != nil {
			return err
		}
	}
	if c.SignalThresholdStr != "" {
		c.signalThreshold, err = scoring.ParseConfidence(c.SignalThresholdStr)
		if err != nil {
			return err
		}
	}
	c.riskScore = scoring.ComputeRiskScore(c.confidence, c.severity)
	return nil
}

func (c *RuleMetadata) References() []string           { return c.ReferencesField }
func (c *RuleMetadata) Severity() scoring.Severity     { return c.severity }
func (c *RuleMetadata) Confidence() scoring.Confidence { return c.confidence }
func (c *RuleMetadata) RiskScore() scoring.RiskScore   { return c.riskScore }
func (c *RuleMetadata) MergeByKeys() []string          { return c.MergeByKeysField }
func (c *RuleMetadata) MergeWindowMins() time.Duration {
	return time.Duration(c.MergeWindowMinsField) * time.Minute
}
func (c *RuleMetadata) ReqSubkeys() []string                { return c.ReqSubkeysField }
func (c *RuleMetadata) Signal() bool                        { return c.SignalField }
func (c *RuleMetadata) SignalThreshold() scoring.Confidence { return c.signalThreshold }
func (c *RuleMetadata) Tags() []string                      { return c.TagsField }
func (c *RuleMetadata) Dispatchers() []string               { return c.DispatchersField }
func (c *RuleMetadata) LogTypes() []string                  { return c.LogTypesField }
func (c *RuleMetadata) Observables() []Observable           { return c.ObservablesField }
func (c *RuleMetadata) Matchers() []string                  { return c.MatchersField }
func (c *RuleMetadata) Formatters() []string                { return c.FormattersField }
func (c *RuleMetadata) Enrichments() []string               { return c.EnrichmentsField }
func (c *RuleMetadata) TuningRules() []string               { return c.TuningRulesField }

// Rule is the full interface for live rule plugins: config accessor + batch evaluation.
// All rules receive a slice of events and return one EvalResult per event.
// plugin.PluginMetadata + Checksum together satisfy plugin.Syncable.
type Rule interface {
	Evaluate(ctx context.Context, evts []events.Event) ([]EvalResult, errors.Error)

	RuleMetadata() *RuleMetadata
	Metadata() plugin.PluginMetadata
	Checksum() string
	String() string
}

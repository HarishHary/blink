// Each rule binary ships alongside a <rule-name>.yaml file that contains the
// full rule configuration.
//
// YAML schema example:
//
//	id: "550e8400-e29b-41d4-a716-446655440000"
//	name: "brute_force_login"
//	display_name: "Brute Force Login Attempt"
//	description: "Detects repeated failed login attempts from a single source."
//	enabled: true
//	version: "1.2.0"
//	severity: "high"
//	confidence: "medium"
//	signal: true
//	signal_threshold: "medium"
//	log_types: ["auth", "cloudtrail"]
//	matchers: ["prod-accounts"]
//	merge_by_keys: ["source_ip", "username"]
//	merge_window_mins: 60
//	req_subkeys: ["source_ip"]
//	tags: ["t1078", "initial-access"]
//	dispatchers: ["pagerduty", "slack"]
//	formatters: ["json-summary"]
//	enrichments: ["geoip"]
//	tuning_rules: ["noisy-hosts"]
//	references: ["https://attack.mitre.org/techniques/T1110/"]

package rules

import (
	"fmt"
	"os"
	"regexp"
	"time"

	cfg "github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/scoring"
	"go.yaml.in/yaml/v4"
)

// ValidationError is an alias so callers in this package use the short name.
type ValidationError = cfg.ValidationError

var semverRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`)

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
	PluginMetadata `yaml:",inline"`

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
func New(c RuleMetadata) (*RuleMetadata, error) {
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

// Registry is the generic registry parameterised for rules.
type RuleRegistry = cfg.Registry[*RuleMetadata]

// Manager is the generic config manager parameterised for rules.
type RuleConfigManager = cfg.ConfigManager[*RuleMetadata]

// Loader implements cfg.Loader[*RuleMetadata] for rules.
// Embed cfg.BaseLoader to inherit default CrossValidate (no-op); override Parse and Validate.
type Loader struct {
	cfg.BaseLoader[RuleMetadata, *RuleMetadata]
}

func (Loader) Parse(path string) (*RuleMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg RuleMetadata
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.resolveScoring(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Validate extends the common structural checks with rule-specific field validation
// (required id, required version, semver format).
func (l Loader) Validate(items []*RuleMetadata, binaries []string) []ValidationError {
	var errs []ValidationError
	for _, cfg := range items {
		name := cfg.Name + ".yaml"
		if cfg.Id == "" {
			errs = append(errs, ValidationError{File: name, Field: "id", Blocking: true, Message: "required field missing"})
		}
		if cfg.Version == "" {
			errs = append(errs, ValidationError{File: name, Field: "version", PluginID: cfg.Id, Blocking: true, Message: "required field missing"})
		} else if !semverRE.MatchString(cfg.Version) {
			errs = append(errs, ValidationError{
				File:     name,
				Field:    "version",
				PluginID: cfg.Id,
				Blocking: true,
				Message:  fmt.Sprintf("%q is not valid semver (expected MAJOR.MINOR.PATCH)", cfg.Version),
			})
		}
	}
	errs = append(errs, l.BaseLoader.Validate(items, binaries)...)
	return errs
}

func NewRuleConfigManager(log *logger.Logger, dir string) *RuleConfigManager {
	return cfg.NewConfigManager[*RuleMetadata](log, "rule", dir, Loader{})
}

// RulesForLogType returns all enabled rules from reg that apply to logType.
// An empty log_types list means the rule applies to all log types.
func RulesForLogType(reg *RuleRegistry, logType string) []*RuleMetadata {
	var result []*RuleMetadata
	for _, cfg := range reg.All() {
		if !cfg.Enabled {
			continue
		}
		if len(cfg.LogTypesField) == 0 {
			result = append(result, cfg)
			continue
		}
		for _, lt := range cfg.LogTypesField {
			if lt == logType {
				result = append(result, cfg)
				break
			}
		}
	}
	return result
}

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
//	file_name: "brute_force_login"
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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harishhary/blink/internal/plugin"
	internal "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/pkg/scoring"
	"go.yaml.in/yaml/v4"
)

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
	SeverityStr       string `yaml:"severity"`
	ConfidenceStr     string `yaml:"confidence"`
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
func Load(path string) (*RuleMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg RuleMetadata
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.resolve(path, data); err != nil {
		return nil, fmt.Errorf("config: validate %s: %w", path, err)
	}

	return &cfg, nil
}

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

// resolve parses string-typed scoring fields, fills defaults, and computes
// the checksum when one is not provided in the YAML.
func (c *RuleMetadata) resolve(path string, _ []byte) error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}

	if err := c.resolveScoring(); err != nil {
		return err
	}

	// Default file_name to the YAML file's base name (without extension).
	if c.FileName == "" {
		base := filepath.Base(path)
		c.FileName = strings.TrimSuffix(base, filepath.Ext(base))
	}

	return nil
}

func (c *RuleMetadata) References() []string               { return c.ReferencesField }
func (c *RuleMetadata) Severity() scoring.Severity         { return c.severity }
func (c *RuleMetadata) Confidence() scoring.Confidence     { return c.confidence }
func (c *RuleMetadata) RiskScore() scoring.RiskScore       { return c.riskScore }
func (c *RuleMetadata) MergeByKeys() []string              { return c.MergeByKeysField }
func (c *RuleMetadata) MergeWindowMins() time.Duration     { return time.Duration(c.MergeWindowMinsField) * time.Minute }
func (c *RuleMetadata) ReqSubkeys() []string               { return c.ReqSubkeysField }
func (c *RuleMetadata) Signal() bool                       { return c.SignalField }
func (c *RuleMetadata) SignalThreshold() scoring.Confidence { return c.signalThreshold }
func (c *RuleMetadata) Tags() []string                     { return c.TagsField }
func (c *RuleMetadata) Dispatchers() []string              { return c.DispatchersField }
func (c *RuleMetadata) LogTypes() []string                 { return c.LogTypesField }
func (c *RuleMetadata) Observables() []Observable          { return c.ObservablesField }
func (c *RuleMetadata) Matchers() []string                 { return c.MatchersField }
func (c *RuleMetadata) Formatters() []string               { return c.FormattersField }
func (c *RuleMetadata) Enrichments() []string              { return c.EnrichmentsField }
func (c *RuleMetadata) TuningRules() []string              { return c.TuningRulesField }

func mergeRouting(a, b internal.RoutingEntry) internal.RoutingEntry {
	out := a
	if b.Mode > a.Mode {
		out.Mode = b.Mode
	}
	if b.RolloutPct > a.RolloutPct {
		out.RolloutPct = b.RolloutPct
	}
	return out
}

type Registry struct {
	byName     map[string]*RuleMetadata
	byID       map[string]*RuleMetadata
	byFileName map[string]*RuleMetadata
	routing    map[string]internal.RoutingEntry // merged routing config per plugin ID
	all        []*RuleMetadata
}

func NewRegistry(dir string) (*Registry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("config: read dir %s: %w", dir, err)
	}

	reg := &Registry{
		byName:     make(map[string]*RuleMetadata),
		byID:       make(map[string]*RuleMetadata),
		byFileName: make(map[string]*RuleMetadata),
		routing:    make(map[string]internal.RoutingEntry),
	}

	var errs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		cfg, err := Load(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		reg.byName[cfg.Name] = cfg
		reg.byFileName[cfg.FileName] = cfg
		if cfg.Id != "" {
			reg.byID[cfg.Id] = cfg
			re := internal.RoutingEntry{
				Mode:       cfg.RolloutMode,
				RolloutPct: cfg.RolloutPct,
			}
			if existing, ok := reg.routing[cfg.Id]; ok {
				reg.routing[cfg.Id] = mergeRouting(existing, re)
			} else {
				reg.routing[cfg.Id] = re
			}
		}
		reg.all = append(reg.all, cfg)
	}

	if len(errs) > 0 {
		return reg, fmt.Errorf("config: %d file(s) failed to load:\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	return reg, nil
}

func (r *Registry) All() []*RuleMetadata                     { return r.all }
func (r *Registry) ByName(name string) *RuleMetadata         { return r.byName[name] }
func (r *Registry) ByID(id string) *RuleMetadata             { return r.byID[id] }
func (r *Registry) ByFileName(fileName string) *RuleMetadata { return r.byFileName[fileName] }

// RoutingByID returns the merged routing config for a plugin ID.
// When multiple YAML sidecars share the same ID, their routing fields are merged
// using max-restrictive semantics (see mergeRouting). The zero value (blue-green,
// no kill switch) is returned when no YAML declares a routing config for this ID.
func (r *Registry) RoutingByID(id string) internal.RoutingEntry { return r.routing[id] }

func (r *Registry) Len() int { return len(r.all) }

// An empty log_types list means the rule applies to all log types.
func (r *Registry) RulesForLogType(logType string) []*RuleMetadata {
	var result []*RuleMetadata
	for _, cfg := range r.all {
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

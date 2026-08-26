package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
)

// TuningRuleMetadata is the in-memory representation of a tuning rule YAML sidecar.
type TuningRuleMetadata struct {
	plugin.Spec   `yaml:",inline"`
	Global        bool   `yaml:"global"`
	RuleTypeStr   string `yaml:"rule_type"`  // "ignore", "set_confidence", "increase_confidence", "decrease_confidence"
	ConfidenceStr string `yaml:"confidence"` // meaningful only for *_confidence rule types

	// Resolved once at load (resolveTyped); yaml:"-" so they aren't (re)serialized.
	RuleType   RuleType           `yaml:"-"`
	Confidence scoring.Confidence `yaml:"-"`
}

// Clone returns an independently owned copy safe to pass across actor boundaries.
func (m *TuningRuleMetadata) Clone() *TuningRuleMetadata {
	if m == nil {
		return nil
	}
	clone := *m
	return &clone
}

// RuleType is the tuning action a rule applies to an alert's confidence.
type RuleType int

// The tuning actions a rule can declare via rule_type.
const (
	Ignore RuleType = iota
	SetConfidence
	IncreaseConfidence
	DecreaseConfidence
)

// TuningRule is the host-side runtime interface for a live tuning-rule plugin.
type TuningRule interface {
	plugin.Artifact
	TuneBatch(ctx context.Context, batch *alerts.Batch) TuneResult
	TuningRuleMetadata() *TuningRuleMetadata
}

// TuneItem holds one alert's tuning outcome and the metadata of the selected plugin version.
type TuneItem struct {
	RuleType   RuleType
	Confidence scoring.Confidence
	Applies    bool
	Err        errors.Error
}

// TuneResult holds the batch-level result from tuning alerts.
type TuneResult struct {
	Items   []TuneItem
	CallErr errors.Error // whole-call failure; never alert-scoped
}

// resolveTyped parses the raw YAML strings into their typed fields; called once at load by the Loader.
func (c *TuningRuleMetadata) resolveTyped() error {
	c.RuleType = parseRuleType(c.RuleTypeStr)
	if c.ConfidenceStr != "" {
		conf, err := scoring.ParseConfidence(c.ConfidenceStr)
		if err != nil {
			return err
		}
		c.Confidence = conf
	}
	return nil
}

// parseRuleType maps the YAML rule_type string to a RuleType (unknown -> Ignore).
func parseRuleType(s string) RuleType {
	switch s {
	case "set_confidence":
		return SetConfidence
	case "increase_confidence":
		return IncreaseConfidence
	case "decrease_confidence":
		return DecreaseConfidence
	default:
		return Ignore
	}
}

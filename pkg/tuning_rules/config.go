// Each tuning rule binary ships alongside a <name>.yaml sidecar file.
//
// YAML schema example:
//
//	id: "550e8400-e29b-41d4-a716-446655440003"
//	name: "noisy-hosts"
//	display_name: "Noisy Hosts Suppressor"
//	description: "Ignores alerts from known-noisy infrastructure hosts."
//	enabled: true
//	version: "1.0.0"
//	file_name: "noisy-hosts"
//	global: false
//	rule_type: "ignore"   # ignore | set_confidence | increase_confidence | decrease_confidence
//	confidence: ""        # only used when rule_type is *_confidence (e.g. "0.8" or "medium")
//	mode: "blue-green"
//	min_procs: 1
//	max_procs: 2

package tuning_rules

import (
	cfg "github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// TuningMetadata is the in-memory representation of a tuning rule YAML sidecar.
type TuningRuleMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	Global                bool   `yaml:"global"`
	RuleType              string `yaml:"rule_type"`  // "ignore", "set_confidence", "increase_confidence", "decrease_confidence"
	Confidence            string `yaml:"confidence"` // meaningful only for *_confidence rule types
}

type TuningRuleConfigManager = cfg.ConfigManager[*TuningRuleMetadata]

// Loader implements cfg.Loader[*TuningMetadata] for tuning rules.
// Embed cfg.BaseLoader to inherit default Parse, Validate, and CrossValidate.
type Loader struct {
	cfg.BaseLoader[TuningRuleMetadata, *TuningRuleMetadata]
}

func NewTuningRuleConfigManager(log *logger.Logger, dir string) *TuningRuleConfigManager {
	return cfg.NewConfigManager[*TuningRuleMetadata](log, "tuning_rule", dir, Loader{})
}

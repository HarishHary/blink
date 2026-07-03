// Each tuning rule binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/tuning_rules-schema.md.

package tuning_rules

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// TuningRuleMetadata is the in-memory representation of a tuning rule YAML sidecar.
type TuningRuleMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	Global                bool   `yaml:"global"`
	RuleType              string `yaml:"rule_type"`  // "ignore", "set_confidence", "increase_confidence", "decrease_confidence"
	Confidence            string `yaml:"confidence"` // meaningful only for *_confidence rule types
}

// TuningRuleSnapshotConfig is the tuning-rule instantiation of cfg.SnapshotConfig.
type TuningRuleSnapshotConfig = config.SnapshotConfig[*TuningRuleMetadata]

// TuningRuleLoader implements cfg.Loader[*TuningRuleMetadata] for tuning rules.
// Embed config.BaseLoader to inherit default Parse, ParseSpec, Validate, and CrossValidate.
type TuningRuleLoader struct {
	config.BaseLoader[TuningRuleMetadata, *TuningRuleMetadata]
}

// NewTuningRuleSnapshotConfig builds the snapshot-backed tuning-rule config, parsing specs with TuningRuleLoader.
func NewTuningRuleSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *TuningRuleSnapshotConfig {
	return config.NewSnapshotConfig[*TuningRuleMetadata](logger, src, TuningRuleLoader{})
}

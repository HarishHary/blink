// Each tuning rule binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/tuning_rules-schema.md.

package tuning_rules

import (
	"fmt"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// SnapshotConfig is the tuning-rule instantiation of cfg.SnapshotConfig.
type SnapshotConfig = config.SnapshotConfig[*TuningRuleMetadata]

// Loader implements cfg.Loader[*TuningRuleMetadata]; Parse/ParseSpec resolve the typed rule_type/confidence fields.
type Loader struct {
	config.BaseLoader[TuningRuleMetadata, *TuningRuleMetadata]
}

// Parse loads a sidecar via the BaseLoader default, then resolves the typed rule_type/confidence fields.
func (l Loader) Parse(path string) (*TuningRuleMetadata, error) {
	cfg, err := l.BaseLoader.Parse(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.resolveTyped(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseSpec is the disk-less counterpart for snapshot-sourced tuning rules; same typed resolution as Parse.
func (l Loader) ParseSpec(name string, spec []byte) (*TuningRuleMetadata, error) {
	cfg, err := l.BaseLoader.ParseSpec(name, spec)
	if err != nil {
		return nil, err
	}
	if err := cfg.resolveTyped(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return cfg, nil
}

// NewSnapshotConfig builds the snapshot-backed tuning-rule config, parsing specs with Loader.
func NewSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *SnapshotConfig {
	return config.NewSnapshotConfig[*TuningRuleMetadata](logger, src, Loader{})
}

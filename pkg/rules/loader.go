// Each rule binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/rules-schema.md.

package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"go.yaml.in/yaml/v4"
)

// ValidationError is an alias so callers in this package use the short name.
type ValidationError = config.ValidationError

var semverRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`)

// Loader implements config.Loader[*RuleMetadata] for rules.
// Embed config.BaseLoader to inherit default Parse, Validate, and CrossValidate.
type RuleLoader struct {
	config.BaseLoader[RuleMetadata, *RuleMetadata]
}

func (r RuleLoader) Parse(path string) (*RuleMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg RuleMetadata
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.SetName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if err := cfg.resolveScoring(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// ParseSpec is the disk-less counterpart to Parse for snapshot-sourced rules: it
// unmarshals the spec and injects name via the BaseLoader default, then applies the
// same rule-specific scoring resolution Parse does.
func (r RuleLoader) ParseSpec(name string, spec []byte) (*RuleMetadata, error) {
	cfg, err := r.BaseLoader.ParseSpec(name, spec)
	if err != nil {
		return nil, err
	}
	if err := cfg.resolveScoring(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return cfg, nil
}

// Validate extends the common structural checks with rule-specific field validation
// (required id, required version, semver format).
func (r RuleLoader) Validate(items []*RuleMetadata, binaries []string) []ValidationError {
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
	errs = append(errs, r.BaseLoader.Validate(items, binaries)...)
	return errs
}

// SnapshotConfig is the rules instantiation of config.SnapshotConfig: a snapshot adapted into the
// data plane's rule catalog + DesiredConfig, fed by any SnapshotSource (a SnapshotReader over Kafka
// or a LocalReader over disk).
type RuleSnapshotConfig = config.SnapshotConfig[*RuleMetadata]

// NewSnapshotConfig builds a rules SnapshotConfig reading from src - a SnapshotReader (Kafka) in
// production, a LocalReader (disk) in dev mode, or a fake in tests - parsing each elected
// artifact's spec with RuleLoader (unmarshal + name injection + scoring resolution).
func NewRuleSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *RuleSnapshotConfig {
	return config.NewSnapshotConfig[*RuleMetadata](logger, src, RuleLoader{})
}

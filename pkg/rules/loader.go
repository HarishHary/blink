// Each rule binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/rules-schema.md.

package rules

import (
	"fmt"
	"regexp"

	"github.com/harishhary/blink/internal/runtime/plugin"
)

var semverRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`)

// Loader parses and validates rule metadata.
type Loader struct {
	plugin.BaseLoader[RuleMetadata, *RuleMetadata]
}

// Parse loads rule metadata from a YAML sidecar on disk.
func (r Loader) Parse(path string) (*RuleMetadata, error) {
	cfg, err := r.BaseLoader.Parse(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.resolveScoring(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseSpec loads rule metadata from a snapshot spec payload.
func (r Loader) ParseSpec(name string, spec []byte) (*RuleMetadata, error) {
	cfg, err := r.BaseLoader.ParseSpec(name, spec)
	if err != nil {
		return nil, err
	}
	if err := cfg.resolveScoring(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return cfg, nil
}

// Validate applies rule-specific validation on top of the shared loader checks.
func (r Loader) Validate(items []*RuleMetadata, binaries []string) []plugin.ValidationError {
	var errs []plugin.ValidationError
	for _, cfg := range items {
		name := cfg.Name + ".yaml"
		if cfg.Id == "" {
			errs = append(errs, plugin.ValidationError{File: name, Field: "id", Blocking: true, Message: "required field missing"})
		}
		if cfg.Version == "" {
			errs = append(errs, plugin.ValidationError{File: name, Field: "version", PluginID: cfg.Id, Blocking: true, Message: "required field missing"})
		} else if !semverRE.MatchString(cfg.Version) {
			errs = append(errs, plugin.ValidationError{
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

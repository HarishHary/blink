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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/harishhary/blink/internal/plugin"
	"go.yaml.in/yaml/v4"
)

// TuningMetadata is the in-memory representation of a tuning rule YAML sidecar.
type TuningMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	Global                bool   `yaml:"global"`
	RuleType              string `yaml:"rule_type"`  // "ignore", "set_confidence", "increase_confidence", "decrease_confidence"
	Confidence            string `yaml:"confidence"` // meaningful only for *_confidence rule types
}

// loader implements plugin.Loader[*TuningMetadata].
type loader struct{}

func (loader) Load(path string) (*TuningMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tuning config: read %s: %w", path, err)
	}
	var cfg TuningMetadata
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("tuning config: parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("tuning config: %s: name is required", path)
	}
	if cfg.FileName == "" {
		base := filepath.Base(path)
		cfg.FileName = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if cfg.Id == "" {
		cfg.Id = cfg.FileName
	}
	return &cfg, nil
}

func (loader) Validate(all []*TuningMetadata) error { return nil }

// Registry and Watcher are the generic implementations parameterised for tuning rules.
type Registry = plugin.Registry[*TuningMetadata]
type Watcher = plugin.Watcher[*TuningMetadata]

func NewRegistry(dir string) (*Registry, error) {
	return plugin.NewRegistry(dir, "tuning_rule", loader{})
}

func NewWatcher(dir string) (*Watcher, error) {
	return plugin.NewWatcher("tuning-config-watcher", dir, "tuning_rule", loader{})
}

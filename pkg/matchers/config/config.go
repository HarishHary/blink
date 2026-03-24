// Each matcher binary ships alongside a <name>.yaml sidecar file.
//
// YAML schema example:
//
//	id: "550e8400-e29b-41d4-a716-446655440002"
//	name: "prod-accounts"
//	display_name: "Production Accounts Matcher"
//	description: "Matches events from production AWS accounts."
//	enabled: true
//	version: "1.0.0"
//	file_name: "prod-accounts"
//	global: false
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

// MatcherMetadata is the in-memory representation of a matcher YAML sidecar.
type MatcherMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	Global                bool `yaml:"global"`
}

// loader implements plugin.Loader[*MatcherMetadata].
type loader struct{}

func (loader) Load(path string) (*MatcherMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("matcher config: read %s: %w", path, err)
	}
	var cfg MatcherMetadata
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("matcher config: parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("matcher config: %s: name is required", path)
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

func (loader) Validate(all []*MatcherMetadata) error { return nil }

// Registry and Watcher are the generic implementations parameterised for matchers.
type Registry = plugin.Registry[*MatcherMetadata]
type Watcher  = plugin.Watcher[*MatcherMetadata]

func NewRegistry(dir string) (*Registry, error) {
	return plugin.NewRegistry(dir, "matcher", loader{})
}

func NewWatcher(dir string) (*Watcher, error) {
	return plugin.NewWatcher("matcher-config-watcher", dir, "matcher", loader{})
}

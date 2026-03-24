// Each formatter binary ships alongside a <name>.yaml sidecar file.
//
// YAML schema example:
//
//	id: "550e8400-e29b-41d4-a716-446655440001"
//	name: "json-summary"
//	display_name: "JSON Summary Formatter"
//	description: "Formats alert data as a structured JSON summary."
//	enabled: true
//	version: "1.0.0"
//	file_name: "json-summary"
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

// FormatterMetadata is the in-memory representation of a formatter YAML sidecar.
type FormatterMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
}

// loader implements plugin.Loader[*FormatterMetadata].
type loader struct{}

func (loader) Load(path string) (*FormatterMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("formatter config: read %s: %w", path, err)
	}
	var cfg FormatterMetadata
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("formatter config: parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("formatter config: %s: name is required", path)
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

func (loader) Validate(all []*FormatterMetadata) error { return nil }

// Registry and Watcher are the generic implementations parameterised for formatters.
type Registry = plugin.Registry[*FormatterMetadata]
type Watcher  = plugin.Watcher[*FormatterMetadata]

func NewRegistry(dir string) (*Registry, error) {
	return plugin.NewRegistry(dir, "formatter", loader{})
}

func NewWatcher(dir string) (*Watcher, error) {
	return plugin.NewWatcher("formatter-config-watcher", dir, "formatter", loader{})
}

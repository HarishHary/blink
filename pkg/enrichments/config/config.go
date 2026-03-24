// Each enrichment binary ships alongside a <name>.yaml sidecar file.
//
// YAML schema example:
//
//	id: "550e8400-e29b-41d4-a716-446655440000"
//	name: "geoip"
//	display_name: "GeoIP Enrichment"
//	description: "Adds geographic location data to events."
//	enabled: true
//	version: "1.0.0"
//	file_name: "geoip"
//	depends_on: ["other-enrichment"]
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

// EnrichmentMetadata is the in-memory representation of an enrichment YAML sidecar.
type EnrichmentMetadata struct {
	plugin.PluginMetadata `yaml:",inline"`
	DependsOn             []string `yaml:"depends_on"`
}

// loader implements plugin.Loader[*EnrichmentMetadata].
type loader struct{}

func (loader) Load(path string) (*EnrichmentMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("enrichment config: read %s: %w", path, err)
	}
	var cfg EnrichmentMetadata
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("enrichment config: parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("enrichment config: %s: name is required", path)
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

func (loader) Validate(all []*EnrichmentMetadata) error {
	index := make(map[string]*EnrichmentMetadata, len(all))
	for _, e := range all {
		index[e.Name] = e
	}
	const (
		unvisited = iota
		inProgress
		done
	)
	state := make(map[string]int, len(all))
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case inProgress:
			return fmt.Errorf("enrichment config: dependency cycle detected: %v → %s", path, name)
		}
		state[name] = inProgress
		e, ok := index[name]
		if !ok {
			return fmt.Errorf("enrichment config: %q depends on unknown enrichment %q", path[len(path)-1], name)
		}
		for _, dep := range e.DependsOn {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, e := range all {
		if err := visit(e.Name, []string{}); err != nil {
			return err
		}
	}
	return nil
}

// Registry and Watcher are the generic implementations parameterised for enrichments.
type Registry = plugin.Registry[*EnrichmentMetadata]
type Watcher  = plugin.Watcher[*EnrichmentMetadata]

func NewRegistry(dir string) (*Registry, error) {
	return plugin.NewRegistry(dir, "enrichment", loader{})
}

func NewWatcher(dir string) (*Watcher, error) {
	return plugin.NewWatcher("enrichment-config-watcher", dir, "enrichment", loader{})
}

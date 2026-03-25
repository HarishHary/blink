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

package enrichments

import (
	"fmt"

	cfg "github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
)

type EnrichmentConfigManager = cfg.ConfigManager[*EnrichmentMetadata]

// Loader implements cfg.Loader[*EnrichmentMetadata] for enrichments.
// Embed cfg.BaseLoader to inherit default Parse and Validate; override CrossValidate.
type Loader struct {
	cfg.BaseLoader[EnrichmentMetadata, *EnrichmentMetadata]
}

// CrossValidate detects dependency cycles across all enrichment sidecars.
func (Loader) CrossValidate(all []*EnrichmentMetadata) error {
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

func NewEnrichmentConfigManager(log *logger.Logger, dir string) *EnrichmentConfigManager {
	return cfg.NewConfigManager[*EnrichmentMetadata](log, "enrichment", dir, Loader{})
}

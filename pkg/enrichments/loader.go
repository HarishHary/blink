// Each enrichment binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/enrichments-schema.md.

package enrichments

import (
	"fmt"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

// SnapshotConfig is the enrichment instantiation of config.SnapshotConfig.
type SnapshotConfig = config.SnapshotConfig[*EnrichmentMetadata]

// Loader implements config.Loader[*EnrichmentMetadata] for enrichments.
// Embed config.BaseLoader to inherit default Parse and Validate; override CrossValidate.
type Loader struct {
	config.BaseLoader[EnrichmentMetadata, *EnrichmentMetadata]
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

// NewSnapshotConfig builds the snapshot-backed enrichment config, parsing specs with Loader.
func NewSnapshotConfig(logger *logger.Logger, src plugin.SnapshotSource) *SnapshotConfig {
	return config.NewSnapshotConfig[*EnrichmentMetadata](logger, src, Loader{})
}

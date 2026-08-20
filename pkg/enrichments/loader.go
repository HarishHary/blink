// Each enrichment binary ships alongside a <name>.yaml sidecar.
// Schema and field reference: docs/internals/schemas/enrichments-schema.md.

package enrichments

import (
	"fmt"

	"github.com/harishhary/blink/internal/runtime/plugin"
)

// Loader implements pluginruntime.Loader[*EnrichmentMetadata] for enrichments.
// Embed pluginruntime.BaseLoader to inherit default Parse and Validate; override CrossValidate.
type Loader struct {
	plugin.BaseLoader[EnrichmentMetadata, *EnrichmentMetadata]
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

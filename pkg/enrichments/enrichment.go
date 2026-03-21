package enrichments

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
)

func ValidateDependencyGraph(enrichments []IEnrichment) error {
	index := make(map[string]IEnrichment, len(enrichments))
	for _, e := range enrichments {
		index[e.Name()] = e
	}

	const (
		unvisited = iota
		inProgress
		done
	)
	state := make(map[string]int, len(enrichments))

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case inProgress:
			return fmt.Errorf("enrichment dependency cycle detected: %v → %s", path, name)
		}
		state[name] = inProgress
		e, ok := index[name]
		if !ok {
			return fmt.Errorf("enrichment %q depends on unknown enrichment %q", path[len(path)-1], name)
		}
		for _, dep := range e.DependsOn() {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}

	for _, e := range enrichments {
		if err := visit(e.Name(), []string{}); err != nil {
			return err
		}
	}
	return nil
}

type IEnrichment interface {
	Enrich(ctx context.Context, alert *alerts.Alert) errors.Error
	// DependsOn returns plugin names that must run before this enrichment.
	DependsOn() []string

	Id() string
	Name() string
	Description() string
	Enabled() bool
	Checksum() string
	String() string
}

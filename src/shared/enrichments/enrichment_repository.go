package enrichments

import (
	"fmt"

	"github.com/harishhary/blink/src/shared/helpers"
)

type EnrichmentRepository struct {
	Enrichments map[string]IEnrichment
	isImported  bool
}

var enrichmentRepository EnrichmentRepository

func init() {
	enrichmentRepository = NewEnrichmentRepository()
}

func GetEnrichmentRepository() *EnrichmentRepository {
	return &enrichmentRepository
}

func NewEnrichmentRepository() EnrichmentRepository {
	return EnrichmentRepository{
		Enrichments: make(map[string]IEnrichment),
		isImported:  false,
	}
}

func (apr *EnrichmentRepository) LoadEnrichments(path string) error {
	var paths = []string{path}
	if !apr.isImported {
		var plugins, err = helpers.LoadPlugins[IEnrichment](paths)
		if err != nil {
			return err
		}
		for _, enrichment := range plugins {
			if err := apr.RegisterEnrichment(enrichment); err != nil {
				return fmt.Errorf("failed to register enrichment: %v", err)
			}
		}
		apr.isImported = true
	}
	return nil
}

func (apr *EnrichmentRepository) GetEnrichment(name string) (IEnrichment, error) {
	if apr.HasEnrichment(name) {
		return apr.Enrichments[name], nil
	}
	return nil, &EnrichmentError{Message: fmt.Sprintf("Enrichment %s not found", name)}
}

func (apr *EnrichmentRepository) HasEnrichment(name string) bool {
	_, exists := apr.Enrichments[name]
	return exists
}

func (apr *EnrichmentRepository) RegisterEnrichment(enrichment IEnrichment) error {
	name := enrichment.Name()
	if _, exists := apr.Enrichments[name]; exists {
		return &EnrichmentError{Message: fmt.Sprintf("Enrichment %s already registered", name)}
	}
	apr.Enrichments[name] = enrichment
	return nil
}

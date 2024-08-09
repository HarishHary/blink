package enrichments

import "github.com/google/uuid"

type EnrichmentOptions func(*Enrichment)

func WithID(id string) EnrichmentOptions {
	return func(enrichment *Enrichment) {
		if id == "" {
			enrichment.id = uuid.NewString()
		} else {
			enrichment.id = id
		}
	}
}

func WithDescription(description string) EnrichmentOptions {
	return func(enrichment *Enrichment) {
		enrichment.description = description
	}
}

func WithEnabled(enabled bool) EnrichmentOptions {
	return func(enrichment *Enrichment) {
		enrichment.enabled = enabled
	}
}

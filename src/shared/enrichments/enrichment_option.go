package enrichments

type EnrichmentOptions func(*Enrichment)

func Description(description string) EnrichmentOptions {
	return func(enrichment *Enrichment) {
		enrichment.description = description
	}
}

func ID(id string) EnrichmentOptions {
	return func(enrichment *Enrichment) {
		enrichment.id = id
	}
}

func Disabled(Disabled bool) EnrichmentOptions {
	return func(enrichment *Enrichment) {
		enrichment.disabled = Disabled
	}
}

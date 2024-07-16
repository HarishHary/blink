package enrichments

type EnrichmentOption func(*Enrichment)

func Name(name string) EnrichmentOption {
	return func(enrichment *Enrichment) {
		enrichment.name = name
	}
}

func Description(description string) EnrichmentOption {
	return func(enrichment *Enrichment) {
		enrichment.description = description
	}
}

func ID(id string) EnrichmentOption {
	return func(enrichment *Enrichment) {
		enrichment.id = id
	}
}

func Disabled(Disabled bool) EnrichmentOption {
	return func(enrichment *Enrichment) {
		enrichment.disabled = Disabled
	}
}

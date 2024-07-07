package enrichments

type EnrichmentOption func(*Enrichment)

func Name(Name string) EnrichmentOption {
	return func(enrichment *Enrichment) {
		enrichment.Name = Name
	}
}

func Description(Description string) EnrichmentOption {
	return func(enrichment *Enrichment) {
		enrichment.Name = Description
	}
}

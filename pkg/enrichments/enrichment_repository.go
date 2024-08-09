package enrichments

import (
	"github.com/harishhary/blink/internal/repository"
)

type EnrichmentRepository struct {
	*repository.Repository[IEnrichment]
}

var enrichmentRepository *EnrichmentRepository

func init() {
	enrichmentRepository = NewEnrichmentRepository()
}

func GetEnrichmentRepository() *EnrichmentRepository {
	return enrichmentRepository
}

func NewEnrichmentRepository() *EnrichmentRepository {
	return &EnrichmentRepository{
		Repository: repository.NewRepository[IEnrichment](),
	}
}

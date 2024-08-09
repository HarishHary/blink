package enrichments

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
)

type IEnrichment interface {
	Enrich(ctx context.Context, alert *alerts.Alert) errors.Error

	// Getters
	Id() string
	Name() string
	Description() string
	Enabled() bool

	// Methods
	String() string
}

type Enrichment struct {
	id          string
	name        string
	description string
	enabled     bool
}

func (e *Enrichment) Id() string {
	return e.id
}

func (e *Enrichment) Name() string {
	return e.name
}

func (e *Enrichment) Description() string {
	return e.description
}

func (e *Enrichment) Enabled() bool {
	return e.enabled
}

func (e *Enrichment) String() string {
	return fmt.Sprintf("Enrichment '%s' with id:'%s', description:'%s', enabled:'%t'", e.name, e.id, e.description, e.enabled)
}

func (e *Enrichment) Enrich(ctx context.Context, alert *alerts.Alert) errors.Error {
	log.Printf("Using enrichment 'base enrichement' with event:'%v'", alert)
	return nil
}

func NewEnrichment(name string, optFns ...EnrichmentOptions) (*Enrichment, errors.Error) {
	if name == "" {
		return nil, errors.New("invalid enrichment options")
	}
	enrichment := &Enrichment{
		id:          uuid.NewString(),
		name:        name,
		description: "Unknown description",
		enabled:     true,
	}
	for _, optFn := range optFns {
		optFn(enrichment)
	}
	return enrichment, nil
}

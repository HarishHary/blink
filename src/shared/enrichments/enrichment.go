package enrichments

import (
	"context"
	"fmt"
	"log"
)

// EnrichmentError custom error for enrichment functions
type EnrichmentError struct {
	Message string
}

func (e *EnrichmentError) Error() string {
	return fmt.Sprintf("Enrichment failed with error: %s", e.Message)
}

type EnrichmentTiming int

const (
	Before EnrichmentTiming = iota
	After
)

type IEnrichment interface {
	Enrich(ctx context.Context, record map[string]interface{}) error
	Name() string
	String() string
}

type Enrichment struct {
	name        string
	id          string
	description string
	disabled    bool
	timing      EnrichmentTiming
}

func (e *Enrichment) Name() string {
	return e.name
}

func (e *Enrichment) String() string {
	return fmt.Sprintf("Enrichment '%s' with id:'%s', description:'%s', disabled:'%t', timing:'%d'", e.name, e.id, e.description, e.disabled, e.timing)
}

func (e *Enrichment) Enrich(ctx context.Context, record map[string]interface{}) error {
	log.Printf("Using enrichment 'base enrichement' with context:'%s' and record:'%s'", ctx, record)
	return nil
}

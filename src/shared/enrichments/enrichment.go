package enrichments

import (
	"context"
	"fmt"
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
}

type Enrichment struct {
	Name         string
	EnrichmentID string
	Description  string
	Disabled     string
	Timing       EnrichmentTiming
}

func (e *Enrichment) Enrich(ctx context.Context, record map[string]interface{}) error {
	return e.EnrichLogic(ctx, record)
}

func (e *Enrichment) EnrichLogic(ctx context.Context, record map[string]interface{}) error {
	return nil
}

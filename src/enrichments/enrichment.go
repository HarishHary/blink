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

type IEnrichmentFunction interface {
	Enrich(ctx context.Context, record map[string]interface{}) error
}

type BaseEnrichmentFunction struct {
	Name   string
	Timing EnrichmentTiming
}

func (e *BaseEnrichmentFunction) Enrich(ctx context.Context, record map[string]interface{}) error {
	return e.EnrichLogic(ctx, record)
}

func (e *BaseEnrichmentFunction) EnrichLogic(ctx context.Context, record map[string]interface{}) error {
	return nil
}

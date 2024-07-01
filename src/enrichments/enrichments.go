package enrichments

import (
	"context"

	"github.com/harishhary/blink/src/events"
)

type EnrichmentTiming int

const (
	Before EnrichmentTiming = iota
	After
)

type IEnrichmentFunction interface {
	Name() string
	Enrich(ctx context.Context, event *events.Event) error
	Timing() EnrichmentTiming
}

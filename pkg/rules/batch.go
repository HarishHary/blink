package rules

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
)

// BatchEvaluator is an optional capability that rules may implement to evaluate
// multiple events in a single call. The rule executor checks for this interface
// via type assertion and prefers it over N individual Evaluate() calls when
// processing a batch of events for the same log type, reducing gRPC round-trips
// for go-plugin rules.
type BatchEvaluator interface {
	EvaluateBatch(ctx context.Context, events []events.Event) ([]bool, errors.Error)
}

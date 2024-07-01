package rules

import (
	"context"

	"github.com/harishhary/blink/src/events"
)

type ITuningRule interface {
	Name() string
	Description() string
	Tune(ctx context.Context, events *events.Event) error
	Severity() int
	Precedence() int
}

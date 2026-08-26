package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
)

// denyEvents never fires - the deny-all matcher already rejects every candidate before this
// would run, so this exists only to give deny-all real matcher-call volume in load tests.
type denyEvents struct{ rules.BaseRule }

func (denyEvents) Evaluate(_ context.Context, _ events.Event) (bool, errors.Error) {
	return false, nil
}

func (denyEvents) AlertTitle(_ events.Event) string { return "Deny event" }

func main() {
	rules.Serve(denyEvents{})
}

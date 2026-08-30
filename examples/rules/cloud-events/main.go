package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
)

// cloudEvents fires on every event routed to it; its log_types filter and the cloud-log-type
// matcher already narrow that to cloud events.
type cloudEvents struct{ rules.BaseRule }

func (cloudEvents) Evaluate(_ context.Context, _ events.Event) (bool, errors.Error) {
	return true, nil
}

func (cloudEvents) AlertTitle(_ events.Event) string { return "Cloud event" }

func main() {
	rules.Serve(cloudEvents{})
}

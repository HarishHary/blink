package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
)

// tenantEvents fires on every event routed to it; the has-tenant matcher already narrows that
// to events carrying a non-empty tenant_id.
type tenantEvents struct{ rules.BaseRule }

func (tenantEvents) Evaluate(_ context.Context, _ events.Event) (bool, errors.Error) {
	return true, nil
}

func (tenantEvents) AlertTitle(_ events.Event) string { return "Tenant event" }

func main() {
	rules.Serve(tenantEvents{})
}

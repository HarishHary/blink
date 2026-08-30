package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

// hasTenant matches events that carry a non-empty tenant_id. Use for testing only.
type hasTenant struct{ matchers.BaseMatcher }

func (hasTenant) Match(_ context.Context, event events.Event) (bool, errors.Error) {
	tenantID, _ := event.Get("tenant_id", "").(string)
	return tenantID != "", nil
}

func main() {
	matchers.Serve(hasTenant{})
}

package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

// denyAll matches no event. Use for testing only.
type denyAll struct{ matchers.BaseMatcher }

func (denyAll) Match(_ context.Context, _ events.Event) (bool, errors.Error) {
	return false, nil
}

func main() {
	matchers.Serve(denyAll{})
}

package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

// allowAllV2 matches every event. Canary candidate for examples/matchers/allow-all. Use for testing only.
type allowAllV2 struct{ matchers.BaseMatcher }

func (allowAllV2) Match(_ context.Context, _ events.Event) (bool, errors.Error) {
	return true, nil
}

func main() {
	matchers.Serve(allowAllV2{})
}

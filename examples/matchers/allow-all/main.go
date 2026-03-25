package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

// allowAll matches every event. Use for testing only.
// All static metadata (name, id, enabled, global, etc.) is declared in
// the companion allow-all.yaml sidecar file.
type allowAll struct{ matchers.BaseMatcher }

func (allowAll) Match(_ context.Context, _ events.Event) (bool, errors.Error) {
	return true, nil
}

func main() {
	matchers.Serve(allowAll{})
}

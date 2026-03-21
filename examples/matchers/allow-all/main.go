package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers/sdk"
)

type allowAll struct{ sdk.BaseMatcher }

func (allowAll) Metadata() sdk.MatcherMetadata {
	return sdk.MatcherMetadata{
		ID:          "allow-all",
		Name:        "Allow All",
		Description: "Matches every event — use for testing only.",
		Enabled:     true,
		Version:     "1.0.0",
	}
}

func (allowAll) Match(_ context.Context, _ events.Event) (bool, errors.Error) {
	return true, nil
}

func main() {
	sdk.Serve(allowAll{})
}

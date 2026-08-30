package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

// cloudLogType matches events whose log_type is "cloud". Use for testing only.
type cloudLogType struct{ matchers.BaseMatcher }

func (cloudLogType) Match(_ context.Context, event events.Event) (bool, errors.Error) {
	return event.Get("log_type", "") == "cloud", nil
}

func main() {
	matchers.Serve(cloudLogType{})
}

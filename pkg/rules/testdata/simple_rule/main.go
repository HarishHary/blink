package main

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
)

type simpleRule struct{ rules.BaseRule }

func (simpleRule) Evaluate(_ context.Context, _ events.Event) (bool, errors.Error) {
	return true, nil
}

func main() {
	rules.Serve(simpleRule{})
}

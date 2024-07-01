package matchers

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/src/events"
)

type IMatchers interface {
	Match(ectx context.Context, vent *events.Event) bool
}

type SimpleMatcher struct{}

func (r *SimpleMatcher) Dispatch(ctx context.Context, event *events.Event) bool {
	fmt.Println("Simple Matcher from:", event.User.UserName)
	return true
}

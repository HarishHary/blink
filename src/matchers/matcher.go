package matchers

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/src/events"
)

type IMatchers interface {
	Match(ectx context.Context, event *events.Event) bool
}

type BaseMatcher struct{}

func (r *BaseMatcher) Match(ctx context.Context, event *events.Event) bool {
	fmt.Println("Simple Matcher from:", event.User.UserName)
	return true
}

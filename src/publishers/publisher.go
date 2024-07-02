package publishers

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/src/events"
)

type IPublishers interface {
	Publish(ctx context.Context, event *events.Event) (*events.Event, error)
}

type BasePublisher struct{}

func (r *BasePublisher) Publish(ctx context.Context, event *events.Event) (*events.Event, error) {
	fmt.Println("Simple Publisher from:", event.User.UserName)
	return event, nil
}

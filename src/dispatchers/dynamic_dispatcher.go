package dispatchers

import (
	"context"

	"github.com/harishhary/blink/src/events"
)

type IDynamicDispatcher interface {
	Dispatch(ctx context.Context, event *events.Event) error // probably a func that return a Dispatcher
}

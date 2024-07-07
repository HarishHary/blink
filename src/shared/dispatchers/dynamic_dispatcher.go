package dispatchers

import (
	"context"
)

type IDynamicDispatcher interface {
	Dispatch(ctx context.Context, record map[string]interface{}) error // probably a func that return a Dispatcher
}

package matchers

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
)

type Matcher interface {
	Id() string
	Name() string
	Description() string
	Enabled() bool
	Checksum() string
	String() string
	Match(ctx context.Context, event events.Event) (bool, errors.Error)
}

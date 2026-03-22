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
	Version() string
	Checksum() string
	String() string
	Match(ctx context.Context, evts []events.Event) ([]bool, errors.Error)
}

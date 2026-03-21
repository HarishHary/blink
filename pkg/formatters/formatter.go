package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
)

type IFormatter interface {
	Format(ctx context.Context, alert *alerts.Alert) (map[string]any, errors.Error)

	Id() string
	Name() string
	Description() string
	Enabled() bool
	Checksum() string
	String() string
}

package formatters

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
)

type IFormatter interface {
	Format(ctx context.Context, alerts []*alerts.Alert) ([]map[string]any, errors.Error)

	Id() string
	Name() string
	Description() string
	Enabled() bool
	Version() string
	Checksum() string
	String() string
}

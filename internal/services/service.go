package services

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
)

// MaxPluginAttempts is the number of DLQ round-trips an alert makes when a referenced
// plugin is missing before the stage passes the alert through without that plugin.
// This prevents infinite DLQ loops while still retrying transient gaps.
const MaxPluginAttempts = 3

type Service interface {
	Name() string
	Run(ctx context.Context) errors.Error
}

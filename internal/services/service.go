package services

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
)

type Service interface {
	Name() string
	Run(ctx context.Context) errors.Error
}

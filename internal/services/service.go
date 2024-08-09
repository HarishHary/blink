package services

import (
	"github.com/harishhary/blink/internal/errors"
)

type Service interface {
	Name() string
	Run() errors.Error
}

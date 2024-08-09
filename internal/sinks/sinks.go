package backends

import "github.com/harishhary/blink/internal/errors"

type Record map[string]any

type ISinks interface {
	Send(data []byte) errors.Error
	SendBatch(data ...[]byte) errors.Error
}

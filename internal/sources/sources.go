package sources

import "github.com/harishhary/blink/internal/errors"

type Record map[string]any

type ISources interface {
	Receive() ([]byte, errors.Error)
	ReceiveBatch(maxEvents int) ([][]byte, errors.Error)
}

package main

import (
	"context"
	"os"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/sdk"
)

type crashingRule struct{ sdk.BaseRule }

func (crashingRule) Evaluate(_ context.Context, _ events.Event) (bool, errors.Error) {
	return false, nil
}

func main() {
	// Exit 300ms after startup — long enough for the manager to complete the
	// Init handshake (~50ms), short enough for crash tests to run quickly.
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Exit(1)
	}()
	sdk.Serve(crashingRule{})
}

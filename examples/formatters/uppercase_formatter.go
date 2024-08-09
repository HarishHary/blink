package formatters

import (
	"context"
	"log"
	"strings"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters"
)

type uppercaseFormatter struct {
	formatters.Formatter
}

func (f *uppercaseFormatter) Format(ctx context.Context, alert alerts.Alert) (bool, errors.Error) {
	log.Printf("Using formatter '%s' with context: '%s' and event: '%s'", f.Name(), ctx, alert.Event)
	if msg, ok := alert.Event["message"].(string); ok {
		alert.Event["message"] = strings.ToUpper(msg)
		return true, nil
	}
	return false, errors.New("message key not found in event")
}

var formatter, _ = formatters.NewFormatter(
	"Uppercase formatter 1",
	formatters.WithDescription("Uppercasing the message..."),
	formatters.WithEnabled(false),
	formatters.WithID(""),
)

var UppercaseFormatter = uppercaseFormatter{
	Formatter: *formatter,
}

var Plugin = UppercaseFormatter

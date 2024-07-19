package formatters

import (
	"context"
	"log"
	"strings"

	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/formatters"
)

type uppercaseFormatter struct {
	formatters.Formatter
}

func (f *uppercaseFormatter) Format(ctx context.Context, record shared.Record) (bool, error) {
	log.Printf("Using formatter '%s' with context: '%s' and record: '%s'", f.Name(), ctx, record)
	if msg, ok := record["message"].(string); ok {
		record["message"] = strings.ToUpper(msg)
		return true, nil
	}
	return false, &formatters.FormatterError{Message: "message key not found in record"}
}

var formatter, _ = formatters.NewFormatter(
	"Uppercase formatter 1",
	formatters.Description("Uppercasing the message..."),
	formatters.Disabled(false),
	formatters.ID(""),
)

var UppercaseFormatter = uppercaseFormatter{
	Formatter: *formatter,
}

package main

import (
	"context"
	"log"
	"strings"

	"github.com/harishhary/blink/src/shared/formatters"
)

type UppercaseFormatter struct {
	formatters.Formatter
}

func (f *UppercaseFormatter) Format(ctx context.Context, record map[string]interface{}) (bool, error) {
	log.Printf("Using formatter '%s' with context: '%s' and record: '%s'", f.Name(), ctx, record)
	if msg, ok := record["message"].(string); ok {
		record["message"] = strings.ToUpper(msg)
		return true, nil
	}
	return false, &formatters.FormatterError{Message: "message key not found in record"}
}

func newUppercaseFormatter(setters ...formatters.FormatterOption) UppercaseFormatter {
	// Default Options
	r := formatters.Formatter{}
	for _, setter := range setters {
		setter(&r)
	}
	return UppercaseFormatter{Formatter: r}
}

// Export the plugin as a symbol
var Plugin = newUppercaseFormatter(
	formatters.Name("Uppercase formatter 1"),
	formatters.Description("Uppercasing the message..."),
	formatters.Disabled(false),
	formatters.ID(""),
)

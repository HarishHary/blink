package formatters

import (
	"context"
	"fmt"
	"log"

	"github.com/harishhary/blink/src/shared"
)

// FormatterError custom error for Formatter
type FormatterError struct {
	Message string
}

func (e *FormatterError) Error() string {
	return fmt.Sprintf("Formatter failed with error: %s", e.Message)
}

type IFormatter interface {
	Format(ctx context.Context, record shared.Record) (bool, error)
	Name() string
	String() string
}

type Formatter struct {
	id          string
	name        string
	description string
	disabled    bool
}

func (f *Formatter) Name() string {
	return f.name
}

func (f *Formatter) String() string {
	return fmt.Sprintf("Formatter '%s' with id:'%s', description:'%s', disabled:'%t'", f.name, f.id, f.description, f.disabled)
}

func (f *Formatter) Format(ctx context.Context, record shared.Record) (bool, error) {
	log.Printf("Using formatter 'base formatter' with context:'%s' and record:'%s'", ctx, record)
	return false, nil
}

func NewFormatter(name string, optFns ...FormatterOptions) (*Formatter, error) {
	if name == "" {
		return nil, &FormatterError{Message: "Invalid Formatter options"}
	}
	formatter := &Formatter{
		name: name,
	}
	for _, optFn := range optFns {
		optFn(formatter)
	}
	return formatter, nil
}

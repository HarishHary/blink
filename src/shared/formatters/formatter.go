package formatters

import (
	"context"
	"fmt"
	"log"
)

// FormatterError custom error for Formatter
type FormatterError struct {
	Message string
}

func (e *FormatterError) Error() string {
	return fmt.Sprintf("Formatter failed with error: %s", e.Message)
}

type IFormatter interface {
	Format(ctx context.Context, record map[string]interface{}) (bool, error)
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

func (f *Formatter) Format(ctx context.Context, record map[string]interface{}) (bool, error) {
	log.Printf("Using formatter 'base formatter' with context:'%s' and record:'%s'", ctx, record)
	return false, nil
}

func NewFormatter(name string, setters ...FormatterOption) Formatter {
	// Default Options
	r := Formatter{
		name: name,
	}
	for _, setter := range setters {
		setter(&r)
	}
	return r
}

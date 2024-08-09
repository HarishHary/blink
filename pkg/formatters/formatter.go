package formatters

import (
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
)

type IFormatter interface {
	Format(alert alerts.Alert) (map[string]any, errors.Error)

	// Getters
	Id() string
	Name() string
	Description() string
	Enabled() bool

	// Methods
	String() string
}

type Formatter struct {
	id          string
	name        string
	description string
	enabled     bool
}

func (f *Formatter) Id() string {
	return f.id
}

func (f *Formatter) Name() string {
	return f.name
}

func (f *Formatter) Description() string {
	return f.description
}

func (f *Formatter) Enabled() bool {
	return f.enabled
}

func (f *Formatter) String() string {
	return fmt.Sprintf("Formatter '%s' with id:'%s', description:'%s', enabled:'%t'", f.name, f.id, f.description, f.enabled)
}

func (f *Formatter) Format(alert alerts.Alert) (*map[string]any, errors.Error) {
	log.Printf("Using formatter 'base formatter' with alert:'%v'", alert)
	return nil, nil
}

func NewFormatter(name string, optFns ...FormatterOptions) (*Formatter, errors.Error) {
	if name == "" {
		return nil, errors.New("invalid formatter options")
	}
	formatter := &Formatter{
		id:          uuid.NewString(),
		name:        name,
		description: "Unknown description",
		enabled:     true,
	}
	for _, optFn := range optFns {
		optFn(formatter)
	}
	return formatter, nil
}

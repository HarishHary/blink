package matchers

import (
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
)

type IMatcher interface {
	Match(event events.Event) (bool, errors.Error)

	// Getters
	Id() string
	Name() string
	Description() string
	Enabled() bool

	// Methods
	String() string
}

type Matcher struct {
	id          string
	name        string
	description string
	enabled     bool
}

func (m *Matcher) Id() string {
	return m.id
}

func (m *Matcher) Name() string {
	return m.name
}

func (m *Matcher) Description() string {
	return m.description
}

func (m *Matcher) Enabled() bool {
	return m.enabled
}

func (m *Matcher) String() string {
	return fmt.Sprintf("Matcher '%s' with id:'%s', description:'%s', enabled:'%t'", m.name, m.id, m.description, m.enabled)
}

func (m *Matcher) Match(event events.Event) (bool, errors.Error) {
	log.Printf("Using matcher 'base matcher' with event:'%s'", event)
	return true, nil
}

func NewMatcher(name string, optFns ...MatcherOptions) (*Matcher, errors.Error) {
	if name == "" {
		return nil, errors.New("invalid matcher options")
	}
	matcher := &Matcher{
		id:          uuid.NewString(),
		name:        name,
		description: "Unknown description",
		enabled:     true,
	}
	for _, optFn := range optFns {
		optFn(matcher)
	}
	return matcher, nil
}

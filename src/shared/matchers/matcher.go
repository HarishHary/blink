package matchers

import (
	"context"
	"fmt"
	"log"
)

// MatcherError custom error for Matcher
type MatcherError struct {
	Message string
}

func (e *MatcherError) Error() string {
	return fmt.Sprintf("Matcher failed with error: %s", e.Message)
}

type IMatcher interface {
	Match(ctx context.Context, record map[string]interface{}) (bool, error)
	Name() string
	String() string
}

type Matcher struct {
	id          string
	name        string
	description string
	disabled    bool
}

func (m *Matcher) Name() string {
	return m.name
}

func (m *Matcher) String() string {
	return fmt.Sprintf("Matcher '%s' with id:'%s', description:'%s', disabled:'%t'", m.name, m.id, m.description, m.disabled)
}

func (m *Matcher) Match(ctx context.Context, record map[string]interface{}) (bool, error) {
	log.Printf("Using matcher 'base matcher' with context:'%s' and record:'%s'", ctx, record)
	return false, nil
}

func NewMatcher(name string, setters ...MatcherOption) Matcher {
	// Default Options
	r := Matcher{
		name: name,
	}
	for _, setter := range setters {
		setter(&r)
	}
	return r
}

package matchers

import (
	"context"
	"fmt"
	"log"

	"github.com/harishhary/blink/src/shared"
)

// MatcherError custom error for Matcher
type MatcherError struct {
	Message string
}

func (e *MatcherError) Error() string {
	return fmt.Sprintf("Matcher failed with error: %s", e.Message)
}

type IMatcher interface {
	Match(ctx context.Context, record shared.Record) (bool, error)
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

func (m *Matcher) Match(ctx context.Context, record shared.Record) (bool, error) {
	log.Printf("Using matcher 'base matcher' with context:'%s' and record:'%s'", ctx, record)
	return false, nil
}

func NewMatcher(name string, optFns ...MatcherOptions) (*Matcher, error) {
	if name == "" {
		return nil, &MatcherError{Message: "Invalid Matcher options"}
	}
	matcher := &Matcher{
		name: name,
	}
	for _, optFn := range optFns {
		optFn(matcher)
	}
	return matcher, nil
}

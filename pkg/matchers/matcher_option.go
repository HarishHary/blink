package matchers

import "github.com/google/uuid"

type MatcherOptions func(*Matcher)

func WithID(id string) MatcherOptions {
	return func(matcher *Matcher) {
		if id == "" {
			matcher.id = uuid.NewString()
		} else {
			matcher.id = id
		}
	}
}

func WithDescription(description string) MatcherOptions {
	return func(matcher *Matcher) {
		matcher.description = description
	}
}

func WithEnabled(enabled bool) MatcherOptions {
	return func(matcher *Matcher) {
		matcher.enabled = enabled
	}
}

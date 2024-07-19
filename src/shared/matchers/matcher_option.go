package matchers

import "github.com/google/uuid"

type MatcherOptions func(*Matcher)

func ID(id string) MatcherOptions {
	return func(matcher *Matcher) {
		if id == "" {
			matcher.id = uuid.NewString()
		} else {
			matcher.id = id
		}
	}
}

func Description(description string) MatcherOptions {
	return func(matcher *Matcher) {
		matcher.description = description
	}
}

func Disabled(disabled bool) MatcherOptions {
	return func(matcher *Matcher) {
		matcher.disabled = disabled
	}
}

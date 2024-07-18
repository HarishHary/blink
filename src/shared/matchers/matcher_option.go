package matchers

import "github.com/google/uuid"

type MatcherOption func(*Matcher)

func ID(id string) MatcherOption {
	return func(matcher *Matcher) {
		if id == "" {
			matcher.id = uuid.NewString()
		} else {
			matcher.id = id
		}
	}
}

func Name(name string) MatcherOption {
	return func(matcher *Matcher) {
		matcher.name = name
	}
}

func Description(description string) MatcherOption {
	return func(matcher *Matcher) {
		matcher.description = description
	}
}

func Disabled(disabled bool) MatcherOption {
	return func(matcher *Matcher) {
		matcher.disabled = disabled
	}
}

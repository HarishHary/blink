package matchers

type MatcherOption func(*Matcher)

func Name(Name string) MatcherOption {
	return func(matcher *Matcher) {
		matcher.Name = Name
	}
}

func Description(Description string) MatcherOption {
	return func(matcher *Matcher) {
		matcher.Description = Description
	}
}

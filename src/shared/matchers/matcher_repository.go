package matchers

import (
	"fmt"

	"github.com/harishhary/blink/src/shared/helpers"
)

type MatcherRepository struct {
	Matchers   map[string]IMatcher
	isImported bool
}

var matcherRepository MatcherRepository

func init() {
	matcherRepository = NewMatcherRepository()
}

func GetMatcherRepository() *MatcherRepository {
	return &matcherRepository
}

func NewMatcherRepository() MatcherRepository {
	return MatcherRepository{
		Matchers:   make(map[string]IMatcher),
		isImported: false,
	}
}

func (apr *MatcherRepository) LoadMatchers(path string) error {
	var paths = []string{path}
	if !apr.isImported {
		var plugins, err = helpers.LoadPlugins[IMatcher](paths)
		if err != nil {
			return err
		}
		for _, matcher := range plugins {
			if err := apr.RegisterMatcher(matcher); err != nil {
				return fmt.Errorf("failed to register matcher: %v", err)
			}
		}
		apr.isImported = true
	}
	return nil
}

func (apr *MatcherRepository) GetMatcher(name string) (IMatcher, error) {
	if apr.HasMatcher(name) {
		return apr.Matchers[name], nil
	}
	return nil, &MatcherError{Message: fmt.Sprintf("Matcher %s not found", name)}
}

func (apr *MatcherRepository) HasMatcher(name string) bool {
	_, exists := apr.Matchers[name]
	return exists
}

func (apr *MatcherRepository) RegisterMatcher(matcher IMatcher) error {
	name := matcher.Name()
	if _, exists := apr.Matchers[name]; exists {
		return &MatcherError{Message: fmt.Sprintf("Matcher %s already registered", name)}
	}
	apr.Matchers[name] = matcher
	return nil
}

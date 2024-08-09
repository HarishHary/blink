package matchers

import (
	"github.com/harishhary/blink/internal/repository"
)

type MatcherRepository struct {
	*repository.Repository[IMatcher]
}

var matcherRepository *MatcherRepository

func init() {
	matcherRepository = NewMatcherRepository()
}

func GetMatcherRepository() *MatcherRepository {
	return matcherRepository
}

func NewMatcherRepository() *MatcherRepository {
	return &MatcherRepository{
		Repository: repository.NewRepository[IMatcher](),
	}
}

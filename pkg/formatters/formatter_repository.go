package formatters

import (
	"github.com/harishhary/blink/internal/repository"
)

type FormatterRepository struct {
	*repository.Repository[IFormatter]
}

var formatterRepository *FormatterRepository

func init() {
	formatterRepository = NewFormatterRepository()
}

func GetFormatterRepository() *FormatterRepository {
	return formatterRepository
}

func NewFormatterRepository() *FormatterRepository {
	return &FormatterRepository{
		Repository: repository.NewRepository[IFormatter](),
	}
}

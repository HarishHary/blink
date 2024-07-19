package formatters

import (
	"fmt"

	"github.com/harishhary/blink/src/shared/helpers"
)

type FormatterRepository struct {
	Formatters map[string]IFormatter
	isImported bool
}

var formatterRepository FormatterRepository

func init() {
	formatterRepository = NewFormatterRepository()
}

func GetFormatterRepository() *FormatterRepository {
	return &formatterRepository
}

func NewFormatterRepository() FormatterRepository {
	return FormatterRepository{
		Formatters: make(map[string]IFormatter),
		isImported: false,
	}
}

func (apr *FormatterRepository) LoadFormatters(path string) error {
	var paths = []string{path}
	if !apr.isImported {
		var plugins, err = helpers.LoadPlugins[IFormatter](paths)
		if err != nil {
			return err
		}
		for _, formatter := range plugins {
			if err := apr.RegisterFormatter(formatter); err != nil {
				return fmt.Errorf("failed to register formatter: %v", err)
			}
		}
		apr.isImported = true
	}
	return nil
}

func (apr *FormatterRepository) GetFormatter(name string) (IFormatter, error) {
	if apr.HasFormatter(name) {
		return apr.Formatters[name], nil
	}
	return nil, &FormatterError{Message: fmt.Sprintf("Formatter %s not found", name)}
}

func (apr *FormatterRepository) HasFormatter(name string) bool {
	_, exists := apr.Formatters[name]
	return exists
}

func (apr *FormatterRepository) RegisterFormatter(formatter IFormatter) error {
	name := formatter.Name()
	if _, exists := apr.Formatters[name]; exists {
		return &FormatterError{Message: fmt.Sprintf("Formatter %s already registered", name)}
	}
	apr.Formatters[name] = formatter
	return nil
}

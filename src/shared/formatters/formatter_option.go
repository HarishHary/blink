package formatters

import "github.com/google/uuid"

type FormatterOption func(*Formatter)

func Name(name string) FormatterOption {
	return func(formatter *Formatter) {
		formatter.name = name
	}
}

func ID(id string) FormatterOption {
	return func(formatter *Formatter) {
		if id == "" {
			formatter.id = uuid.NewString()
		} else {
			formatter.id = id
		}
	}
}

func Description(description string) FormatterOption {
	return func(formatter *Formatter) {
		formatter.description = description
	}
}

func Disabled(disabled bool) FormatterOption {
	return func(formatter *Formatter) {
		formatter.disabled = disabled
	}
}

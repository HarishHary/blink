package formatters

import "github.com/google/uuid"

type FormatterOptions func(*Formatter)

func ID(id string) FormatterOptions {
	return func(formatter *Formatter) {
		if id == "" {
			formatter.id = uuid.NewString()
		} else {
			formatter.id = id
		}
	}
}

func Description(description string) FormatterOptions {
	return func(formatter *Formatter) {
		formatter.description = description
	}
}

func Disabled(disabled bool) FormatterOptions {
	return func(formatter *Formatter) {
		formatter.disabled = disabled
	}
}

package formatters

import "github.com/google/uuid"

type FormatterOptions func(*Formatter)

func WithID(id string) FormatterOptions {
	return func(formatter *Formatter) {
		if id == "" {
			formatter.id = uuid.NewString()
		} else {
			formatter.id = id
		}
	}
}

func WithDescription(description string) FormatterOptions {
	return func(formatter *Formatter) {
		formatter.description = description
	}
}

func WithEnabled(enabled bool) FormatterOptions {
	return func(formatter *Formatter) {
		formatter.enabled = enabled
	}
}

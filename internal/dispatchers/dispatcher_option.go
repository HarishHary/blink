package dispatchers

type DispatcherOptions func(*Dispatcher)

func WithID(id string) DispatcherOptions {
	return func(dispatcher *Dispatcher) {
		dispatcher.id = id
	}
}

func WithDescription(description string) DispatcherOptions {
	return func(dispatcher *Dispatcher) {
		dispatcher.description = description
	}
}

func WithConfig(config map[string]any) DispatcherOptions {
	return func(dispatcher *Dispatcher) {
		dispatcher.config = config
	}
}

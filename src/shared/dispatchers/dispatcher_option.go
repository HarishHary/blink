package dispatchers

type DispatcherOption func(*Dispatcher)

func Name(name string) DispatcherOption {
	return func(dispatcher *Dispatcher) {
		dispatcher.name = name
	}
}

func ID(id string) DispatcherOption {
	return func(dispatcher *Dispatcher) {
		dispatcher.id = id
	}
}

func URL(url string) DispatcherOption {
	return func(dispatcher *Dispatcher) {
		dispatcher.url = url
	}
}

func Config(config map[string]any) DispatcherOption {
	return func(dispatcher *Dispatcher) {
		dispatcher.config = config
	}
}

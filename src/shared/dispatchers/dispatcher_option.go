package dispatchers

type DispatcherOptions func(*Dispatcher)

func ID(id string) DispatcherOptions {
	return func(dispatcher *Dispatcher) {
		dispatcher.id = id
	}
}

func URL(url string) DispatcherOptions {
	return func(dispatcher *Dispatcher) {
		dispatcher.url = url
	}
}

func Config(config map[string]any) DispatcherOptions {
	return func(dispatcher *Dispatcher) {
		dispatcher.config = config
	}
}

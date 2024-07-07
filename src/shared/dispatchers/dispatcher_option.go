package dispatchers

type DispatcherOption func(*Dispatcher)

func Name(Name string) DispatcherOption {
	return func(dispatcher *Dispatcher) {
		dispatcher.Name = Name
	}
}

func URL(URL string) DispatcherOption {
	return func(dispatcher *Dispatcher) {
		dispatcher.URL = URL
	}
}

func Config(Config map[string]interface{}) DispatcherOption {
	return func(dispatcher *Dispatcher) {
		dispatcher.Config = Config
	}
}

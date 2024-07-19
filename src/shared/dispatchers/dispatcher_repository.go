package dispatchers

import "fmt"

type DispatcherRepository struct {
	Dispatchers map[string]IDispatcher
	isImported  bool
}

var dispatcherRepository DispatcherRepository

func init() {
	dispatcherRepository = NewDispatcherRepository()
}

func NewDispatcherRepository() DispatcherRepository {
	return DispatcherRepository{
		Dispatchers: make(map[string]IDispatcher),
		isImported:  false,
	}
}

func GetDispatcherRepository() *DispatcherRepository {
	return &dispatcherRepository
}

func (dpr *DispatcherRepository) GetDispatcher(name string) (IDispatcher, error) {
	if dpr.HasDispatcher(name) {
		return dpr.Dispatchers[name], nil
	}
	return nil, &DispatcherError{Message: fmt.Sprintf("Dispatcher %s not found", name)}
}

func (dpr *DispatcherRepository) HasDispatcher(name string) bool {
	// apr.ImportDispatcher()
	_, exists := dpr.Dispatchers[name]
	return exists
}

func (dpr *DispatcherRepository) RegisterDispatcher(dispatcher IDispatcher) error {
	name := dispatcher.Name()
	if _, exists := dpr.Dispatchers[name]; exists {
		return &DispatcherError{Message: fmt.Sprintf("Dispatcher %s already registered", name)}
	}
	dpr.Dispatchers[name] = dispatcher
	return nil
}

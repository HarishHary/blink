package dispatchers

import "fmt"

type DispatcherConstructor func(config map[string]string) (IDispatcher, error)

type DispatcherRepository struct {
	Dispatchers map[string]DispatcherConstructor
	isImported  bool
}

var dispatcherRepository DispatcherRepository

func init() {
	dispatcherRepository = NewDispatcherRepository()
}

func NewDispatcherRepository() DispatcherRepository {
	return DispatcherRepository{
		Dispatchers: make(map[string]DispatcherConstructor),
		isImported:  false,
	}
}

func GetDispatcherRepository() *DispatcherRepository {
	return &dispatcherRepository
}

func (dpr *DispatcherRepository) GetDispatcher(name string) (DispatcherConstructor, error) {
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

func (dpr *DispatcherRepository) RegisterDispatcher(name string, constructor DispatcherConstructor) error {
	if _, exists := dpr.Dispatchers[name]; exists {
		return &DispatcherError{Message: fmt.Sprintf("Dispatcher %s already registered", name)}
	}
	dpr.Dispatchers[name] = constructor
	return nil
}

func (dpr *DispatcherRepository) CreateDispatcher(name string, config map[string]string) (IDispatcher, error) {
	constructor, ok := dpr.Dispatchers[name]
	if !ok {
		return nil, &DispatcherError{Message: fmt.Sprintf("Dispatcher %s not found", name)}
	}
	return constructor(config)
}

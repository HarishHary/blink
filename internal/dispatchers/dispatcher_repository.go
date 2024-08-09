package dispatchers

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/harishhary/blink/internal/errors"
	"gopkg.in/yaml.v2"
)

type DispatcherRepository struct {
	sync.RWMutex
	Dispatchers map[string]IDispatcher
}

var dispatcherRepository DispatcherRepository

func init() {
	dispatcherRepository = NewDispatcherRepository()
}

func GetDispatcherRepository() *DispatcherRepository {
	return &dispatcherRepository
}

func NewDispatcherRepository() DispatcherRepository {
	return DispatcherRepository{
		RWMutex:     sync.RWMutex{},
		Dispatchers: make(map[string]IDispatcher),
	}
}

func (dpr *DispatcherRepository) GetDispatcher(name string) (IDispatcher, errors.Error) {
	if dpr.HasDispatcher(name) {
		dpr.RLock()
		defer dpr.RUnlock()
		return dpr.Dispatchers[name], nil
	}
	return nil, errors.NewF("dispatcher %s not found", name)
}

func (dpr *DispatcherRepository) HasDispatcher(name string) bool {
	dpr.RLock()
	defer dpr.RUnlock()
	_, exists := dpr.Dispatchers[name]
	return exists
}

func (dpr *DispatcherRepository) RegisterDispatcher(dispatcher IDispatcher) errors.Error {
	name := dispatcher.Name()
	if dpr.HasDispatcher(name) {
		return errors.NewF("dispatcher %s already registered", name)
	}
	dpr.Lock()
	defer dpr.Unlock()
	dpr.Dispatchers[name] = dispatcher
	return nil
}

func (dpr *DispatcherRepository) UnregisterDispatcher(name string) errors.Error {
	if dpr.HasDispatcher(name) {
		dpr.Lock()
		defer dpr.Unlock()
		delete(dpr.Dispatchers, name)
		return nil
	}
	return errors.NewF("dispatcher %s not found", name)
}

func (dpr *DispatcherRepository) LoadDispatcher(filePath string) errors.Error {
	dpr.Lock()
	defer dpr.Unlock()

	data, err := os.ReadFile(filePath)
	if err != nil {
		return errors.NewF("failed to read YAML file: %s", err)
	}

	var config map[string]map[string]map[string]interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return errors.NewF("failed to parse YAML config: %s", err)
	}

	dispatchers := make(map[string]IDispatcher)

	for serviceName, serviceConfig := range config {
		constructor, exists := dispatcherConstructors[serviceName]
		if !exists {
			return errors.NewF("no constructor registered for service %s", serviceName)
		}
		for rootKey, conf := range serviceConfig {
			dispatcherName := fmt.Sprintf("%s:%s", serviceName, rootKey)

			var dispatcher IDispatcher

			dispatcher, err := constructor(dispatcherName, conf)
			if err != nil {
				return errors.NewF("failed to create dispatcher %s: %s", dispatcherName, err)
			}

			dispatchers[dispatcherName] = dispatcher
		}
	}

	for _, dispatcher := range dispatchers {
		if err := dpr.RegisterDispatcher(dispatcher); err != nil {
			return errors.New(err)
		}
	}
	return nil
}

// Function to load dispatchers from all YAML files in a directory
func (dpr *DispatcherRepository) LoadDispatchers(directoryPath string) errors.Error {
	files, err := filepath.Glob(filepath.Join(directoryPath, "*.yaml"))
	if err != nil {
		return errors.NewF("failed to list YAML files in directory %s: %s", directoryPath, err)
	}

	var loadErrors []errors.Error
	for _, file := range files {
		if err := dpr.LoadDispatcher(file); err != nil {
			loadErrors = append(loadErrors, errors.New(err))
			continue
		}
	}
	if len(loadErrors) > 0 {
		for _, e := range loadErrors {
			log.Println(e)
		}
	}
	return nil
}

// DispatcherConstructor is a function that creates a new dispatcher
type DispatcherConstructor func(name string, config map[string]any) (IDispatcher, errors.Error)

var dispatcherConstructors = make(map[string]DispatcherConstructor)

func RegisterDispatcherConstructor(serviceName string, constructor DispatcherConstructor) {
	dispatcherConstructors[serviceName] = constructor
}

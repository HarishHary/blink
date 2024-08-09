package repository

import (
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/messaging"
)

// Meta type for all entities that can be synced
type ISyncable interface {
	Name() string
	Description() string
	Enabled() bool
}

type IRepository[T ISyncable] interface {
	Load(directoryPath string) errors.Error
	Has(name string) bool
	Unregister(name string) errors.Error
	Record(newmessage messaging.Message)
	Get(name string) (T, errors.Error)
	Register(item T) errors.Error
	Diff(targetRepo *Repository[T]) (toAdd []T, toDelete []string)
}

type Repository[T ISyncable] struct {
	Data map[string]T
	sync.RWMutex
}

func NewRepository[T ISyncable]() *Repository[T] {
	return &Repository[T]{
		Data: make(map[string]T),
	}
}

func (repo *Repository[T]) Load(directoryPath string) errors.Error {
	var loadErrors []errors.Error
	err := filepath.Walk(directoryPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			loadErrors = append(loadErrors, errors.NewF("failed to load plugin from %s with err %s", path, err))
			return nil
		}
		if !info.IsDir() && filepath.Ext(path) == ".so" {
			var obj, err = helpers.LoadPlugin[T](path)

			if err != nil {
				loadErrors = append(loadErrors, errors.New(err))
				return nil
			}
			if err := repo.Register(obj); err != nil {
				loadErrors = append(loadErrors, errors.New(err))
				return nil
			}
			return nil
		}
		return nil
	})
	if err != nil {
		return errors.NewF("failed to walk through directory: %s", err)
	}
	if len(loadErrors) > 0 {
		for _, e := range loadErrors {
			log.Println(e)
		}
		return loadErrors[0] // Return the first load error for simplicity
	}
	return nil
}

func (repo *Repository[T]) Diff(targetRepo *Repository[T]) (toAdd []T, toDelete []string) {
	repo.RLock()
	defer repo.RUnlock()
	targetRepo.RLock()
	defer targetRepo.RUnlock()

	toAdd, toDelete = []T{}, []string{}

	for name, item := range targetRepo.Data {
		if _, exists := repo.Data[name]; !exists {
			toAdd = append(toAdd, item)
		}
	}

	for name := range repo.Data {
		if _, exists := targetRepo.Data[name]; !exists {
			toDelete = append(toDelete, name)
		}
	}

	return toAdd, toDelete
}

func (repo *Repository[T]) Has(name string) bool {
	repo.RLock()
	defer repo.RUnlock()
	_, exists := repo.Data[name]
	return exists
}

func (repo *Repository[T]) Get(name string) (T, errors.Error) {
	if repo.Has(name) {
		repo.RLock()
		defer repo.RUnlock()
		return repo.Data[name], nil
	}
	var zeroValue T
	return zeroValue, errors.NewF("%s not found", name)
}

func (repo *Repository[T]) Register(item T) errors.Error {
	name := item.Name()
	if repo.Has(name) {
		return errors.NewF("%s already registered", name)
	}
	repo.Lock()
	defer repo.Unlock()
	repo.Data[name] = item
	return nil
}

func (repo *Repository[T]) Unregister(name string) errors.Error {
	if repo.Has(name) {
		repo.Lock()
		defer repo.Unlock()
		delete(repo.Data, name)
		return nil
	}
	return errors.NewF("%s not found", name)
}

func (repo *Repository[T]) Record(newmessage messaging.Message) {
	switch newmessage := newmessage.(type) {
	case RegisterMessage[T]:
		repo.Register(newmessage.Item)
	case UnregisterMessage[T]:
		repo.Unregister(newmessage.ItemID)
	}
}

type RegisterMessage[T ISyncable] struct {
	messaging.Message
	Item T
}

type UnregisterMessage[T ISyncable] struct {
	messaging.Message
	ItemID string
}

func NewRegisterMessage[T ISyncable](item T) RegisterMessage[T] {
	return RegisterMessage[T]{Item: item}
}

func NewUnregisterMessage[T ISyncable](itemID string) UnregisterMessage[T] {
	return UnregisterMessage[T]{ItemID: itemID}
}

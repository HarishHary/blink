package sync

import (
	"log"
	"time"

	"github.com/harishhary/blink/cmd/alert_processor/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/repository"
	"github.com/harishhary/blink/internal/sources/azure_storage"
	"github.com/harishhary/blink/pkg/formatters"
)

type SyncService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
	storage      *azure_storage.Client
}

func New() *SyncService {
	serviceContext := context.New("BLINK-ALERT-PROCESSOR - SYNC")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	storageContext := azure_storage.Configuration{}
	if err := configuration.LoadFromEnvironment(&storageContext); err != nil {
		log.Fatalln(err)
	}
	storage := azure_storage.New(storageContext, "formatters")
	return &SyncService{
		ServiceContext: serviceContext,
		syncMessages:   serviceContext.Messages().Subscribe(message.SyncService, false),
		storage:        storage,
	}
}

func (service *SyncService) Run() errors.Error {
	service.Info("getting formatter repository...")
	formatterRepository := formatters.GetFormatterRepository()

	service.Info("loading formatter repository...")
	formatterDirectory := "/Users/harish.segar/Documents/Research/blink/examples/formatters/"
	formatterRepository.Load(formatterDirectory)

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}

		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			formatterRepository.Record(newMessage)
		}
	}()

	for {
		service.Info("syncing formatter repository...")
		time.Sleep(10 * time.Second)

		tempRepo := repository.NewRepository[formatters.IFormatter]()
		if err := tempRepo.Load(formatterDirectory); err != nil {
			return errors.NewE(err)
		}
		service.Debug("running diff for formatters")
		toAdd, toDelete := formatterRepository.Diff(tempRepo)

		if len(toAdd) == 0 && len(toDelete) == 0 {
			service.Debug("no diff detected for formatters")
			return nil
		}

		service.Info("%d formatter to add", len(toAdd))
		service.Info("%d formatter to delete", len(toDelete))

		for _, entry := range toAdd {
			service.Debug("publishing register message for '%s'\n", entry.Name())
			service.Messages().Publish(message.SyncService, repository.NewRegisterMessage[formatters.IFormatter](entry))
		}
		for _, instanceID := range toDelete {
			service.Debug("publishing unregister message for '%s'\n", instanceID)
			service.Messages().Publish(message.SyncService, repository.NewUnregisterMessage[formatters.IFormatter](instanceID))
		}
	}
}

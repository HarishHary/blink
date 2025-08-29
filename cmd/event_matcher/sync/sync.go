package sync

import (
	"log"
	"os"
	"time"

	"github.com/harishhary/blink/cmd/event_matcher/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/repository"
	"github.com/harishhary/blink/pkg/matchers"
)

// SyncService hot-loads matcher plugins and broadcasts register/unregister messages.
type SyncService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
}

// New constructs an event-matcher sync service.
func New() *SyncService {
	serviceContext := context.New("BLINK-EVENT-MATCHER - SYNC")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	return &SyncService{
		ServiceContext: serviceContext,
		syncMessages:   serviceContext.Messages().Subscribe(message.SyncService, false),
	}
}

// Name returns the sync service name.
func (service *SyncService) Name() string { return "event-matcher-sync" }

// Run begins hot-loading matcher plugins and syncing with the plugin directory.
func (service *SyncService) Run() errors.Error {
	matcherRepo := matchers.GetMatcherRepository()
	matcherDir := os.Getenv("MATCHER_PLUGIN_DIR")
	matcherRepo.Load(matcherDir)

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}
		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			matcherRepo.Record(newMessage)
		}
	}()

	for {
		service.Info("syncing matcher plugins...")
		time.Sleep(10 * time.Second)

		tempRepo := repository.NewRepository[matchers.IMatcher]()
		if err := tempRepo.Load(matcherDir); err != nil {
			service.Error(err)
			continue
		}
		toAdd, toDelete := matcherRepo.Diff(tempRepo)
		if len(toAdd) == 0 && len(toDelete) == 0 {
			continue
		}
		service.Info("%d matcher(s) to add", len(toAdd))
		service.Info("%d matcher(s) to delete", len(toDelete))
		for _, entry := range toAdd {
			service.Debug("publishing register message for '%s'", entry.Name())
			service.Messages().Publish(message.SyncService, repository.NewRegisterMessage[matchers.IMatcher](entry))
		}
		for _, instanceID := range toDelete {
			service.Debug("publishing unregister message for '%s'", instanceID)
			service.Messages().Publish(message.SyncService, repository.NewUnregisterMessage[matchers.IMatcher](instanceID))
		}
	}
}

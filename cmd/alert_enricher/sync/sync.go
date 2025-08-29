package sync

import (
	"log"
	"os"
	"time"

	"github.com/harishhary/blink/cmd/alert_enricher/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/repository"
	"github.com/harishhary/blink/pkg/enrichments"
)

// SyncService hot-loads enrichment plugins and broadcasts register/unregister messages.
type SyncService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
}

// New constructs the alert-enricher sync service.
func New() *SyncService {
	serviceContext := context.New("BLINK-ALERT-ENRICHER - SYNC")
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
func (service *SyncService) Name() string { return "alert-enricher-sync" }

// Run begins hot-loading enrichment plugins and syncing with the plugin directory.
func (service *SyncService) Run() errors.Error {
	enricherRepo := enrichments.GetEnrichmentRepository()
	enrichDir := os.Getenv("ENRICHER_PLUGIN_DIR")
	enricherRepo.Load(enrichDir)

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}
		for {
			newMsg := recv()
			service.Debug("recording new message: '%v'", newMsg)
			enricherRepo.Record(newMsg)
		}
	}()

	for {
		service.Info("syncing enrichment plugins...")
		time.Sleep(10 * time.Second)

		tempRepo := repository.NewRepository[enrichments.IEnrichment]()
		if err := tempRepo.Load(enrichDir); err != nil {
			service.Error(err)
			continue
		}
		toAdd, toDelete := enricherRepo.Diff(tempRepo)
		if len(toAdd) == 0 && len(toDelete) == 0 {
			continue
		}
		service.Info("%d enricher(s) to add", len(toAdd))
		service.Info("%d enricher(s) to delete", len(toDelete))
		for _, entry := range toAdd {
			service.Debug("publishing register message for '%s'", entry.Name())
			service.Messages().Publish(message.SyncService, repository.NewRegisterMessage[enrichments.IEnrichment](entry))
		}
		for _, id := range toDelete {
			service.Debug("publishing unregister message for '%s'", id)
			service.Messages().Publish(message.SyncService, repository.NewUnregisterMessage[enrichments.IEnrichment](id))
		}
	}
}

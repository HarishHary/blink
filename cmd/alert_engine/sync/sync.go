package sync

import (
	"log"
	"time"

	"github.com/harishhary/blink/cmd/alert_engine/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/repository"
	"github.com/harishhary/blink/internal/sources/azure_storage"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/rules/tuning_rules"
)

// Periodically sync the loaded rules with the database
type SyncService struct {
	context.ServiceContext
	syncMessages       messaging.MessageQueue
	tuningRulesStorage *azure_storage.Client
	enrichmentStorage  *azure_storage.Client
}

func New() *SyncService {
	serviceContext := context.New("BLINK-ALERT-ENGINE - SYNC")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	storageContext := azure_storage.Configuration{}
	if err := configuration.LoadFromEnvironment(&storageContext); err != nil {
		log.Fatalln(err)
	}
	tuningRulesStorage := azure_storage.New(storageContext, "tuning-rules")
	enrichmentStorage := azure_storage.New(storageContext, "enrichments")
	return &SyncService{
		ServiceContext:     serviceContext,
		syncMessages:       serviceContext.Messages().Subscribe(message.SyncService, false),
		tuningRulesStorage: tuningRulesStorage,
		enrichmentStorage:  enrichmentStorage,
	}
}

func SyncRepositories[T repository.ISyncable](service *SyncService, directory string, repo repository.IRepository[T]) errors.Error {
	repoType := "tuning rules"
	switch repo.(type) {
	case repository.IRepository[enrichments.IEnrichment]:
		repoType = "enrichments"
	case repository.IRepository[tuning_rules.ITuningRule]:
		repoType = "tuning rules"
	}

	tempRepo := repository.NewRepository[T]()
	if err := tempRepo.Load(directory); err != nil {
		return errors.NewE(err)
	}
	service.Debug("running diff for %s", repoType)
	toAdd, toDelete := repo.Diff(tempRepo)

	if len(toAdd) == 0 && len(toDelete) == 0 {
		service.Debug("no diff detected for %s", repoType)
		return nil
	}

	service.Info("%d %s to add", len(toAdd), repoType)
	service.Info("%d %s to delete", len(toDelete), repoType)

	for _, entry := range toAdd {
		service.Debug("publishing register message for '%s'\n", entry.Name())
		service.Messages().Publish(message.SyncService, repository.NewRegisterMessage[T](entry))
	}
	for _, instanceID := range toDelete {
		service.Debug("publishing unregister message for '%s'\n", instanceID)
		service.Messages().Publish(message.SyncService, repository.NewUnregisterMessage[T](instanceID))
	}
	return nil
}

func (service *SyncService) Run() errors.Error {
	service.Info("getting repositories...")
	enrichmentRepository := enrichments.GetEnrichmentRepository()
	tuningRuleRepository := tuning_rules.GetTuningRuleRepository()

	service.Info("loading repositories...")
	enrichmentDirectory := "/Users/harish.segar/Documents/Research/blink/examples/enrichments/"
	enrichmentRepository.Load(enrichmentDirectory)

	tuningRuleDirectory := "/Users/harish.segar/Documents/Research/blink/examples/tuning-rules/"
	tuningRuleRepository.Load(tuningRuleDirectory)

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}

		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			enrichmentRepository.Record(newMessage)
			tuningRuleRepository.Record(newMessage)
		}
	}()

	for {
		service.Info("syncing repositories...")
		time.Sleep(10 * time.Second)

		if err := SyncRepositories[enrichments.IEnrichment](service, enrichmentDirectory, enrichmentRepository); err != nil {
			service.Error(err)
		}

		if err := SyncRepositories[tuning_rules.ITuningRule](service, tuningRuleDirectory, tuningRuleRepository); err != nil {
			service.Error(err)
		}
	}
}

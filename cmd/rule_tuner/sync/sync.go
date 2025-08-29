package sync

import (
	"log"
	"os"
	"time"

	"github.com/harishhary/blink/cmd/rule_tuner/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/repository"
	"github.com/harishhary/blink/pkg/rules/tuning_rules"
)

// SyncService hot-loads tuning-rule plugins and broadcasts registration messages.
// SyncService hot-loads tuning-rule plugins and broadcasts registration messages.
type SyncService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
}

// New constructs the rule-tuner sync service.
// New constructs the rule-tuner sync service.
func New() *SyncService {
	serviceContext := context.New("BLINK-RULE-TUNER - SYNC")
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
// Name returns the sync service name.
func (service *SyncService) Name() string { return "rule-tuner-sync" }

// Run blocks and manages plugin hot-loading for tuning rules.
// Run begins hot-loading tuning-rule plugins and syncing with the plugin directory.
func (service *SyncService) Run() errors.Error {
	tunerRepo := tuning_rules.GetTuningRuleRepository()
	tuneDir := os.Getenv("TUNER_PLUGIN_DIR")
	tunerRepo.Load(tuneDir)

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}
		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			tunerRepo.Record(newMessage)
		}
	}()

	for {
		service.Info("syncing tuning-rule plugins...")
		time.Sleep(10 * time.Second)

		tempRepo := repository.NewRepository[tuning_rules.ITuningRule]()
		if err := tempRepo.Load(tuneDir); err != nil {
			service.Error(err)
			continue
		}
		toAdd, toDelete := tunerRepo.Diff(tempRepo)
		if len(toAdd) == 0 && len(toDelete) == 0 {
			continue
		}
		service.Info("%d tuner(s) to add", len(toAdd))
		service.Info("%d tuner(s) to delete", len(toDelete))
		for _, entry := range toAdd {
			service.Debug("publishing register message for '%s'", entry.Name())
			service.Messages().Publish(message.SyncService, repository.NewRegisterMessage[tuning_rules.ITuningRule](entry))
		}
		for _, instanceID := range toDelete {
			service.Debug("publishing unregister message for '%s'", instanceID)
			service.Messages().Publish(message.SyncService, repository.NewUnregisterMessage[tuning_rules.ITuningRule](instanceID))
		}
	}
}

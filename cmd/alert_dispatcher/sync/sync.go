package sync

import (
	"log"
	"os"
	"time"

	"github.com/harishhary/blink/cmd/alert_dispatcher/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
)

// SyncService hot-loads dispatcher configurations and broadcasts register/unregister messages.
type SyncService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
}

// New constructs the alert-dispatcher sync service.
func New() *SyncService {
	serviceContext := context.New("BLINK-ALERT-DISPATCHER - SYNC")
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
func (service *SyncService) Name() string { return "alert-dispatcher-sync" }

// Run loads dispatcher configs from YAML and publishes sync messages.
func (service *SyncService) Run() errors.Error {
	dispatcherRepo := dispatchers.GetDispatcherRepository()
	configDir := os.Getenv("DISPATCHER_CONFIG_DIR")
	for {
		service.Info("loading dispatcher configs...")
		if err := dispatcherRepo.LoadDispatchers(configDir); err != nil {
			service.Error(err)
		}
		time.Sleep(10 * time.Second)
	}
}

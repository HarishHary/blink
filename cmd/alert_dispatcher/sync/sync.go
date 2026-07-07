package sync

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
)

type SyncService struct {
	*logger.Logger
	dispatcherRepo *dispatchers.DispatcherRepository
	configDir      string
}

// Config is the explicit set of dependencies New needs, injected by main.
type Config struct {
	ConfigDir string
}

func New(c Config, dispatcherRepo *dispatchers.DispatcherRepository) *SyncService {
	return &SyncService{
		Logger:         logger.New("alert-dispatcher-sync", "dev"),
		dispatcherRepo: dispatcherRepo,
		configDir:      c.ConfigDir,
	}
}

func (service *SyncService) Name() string { return "alert-dispatcher-sync" }

func (service *SyncService) Run(ctx context.Context) errors.Error {
	for {
		service.Info("loading dispatcher configs from %s", service.configDir)
		if err := service.dispatcherRepo.LoadDispatchers(service.configDir); err != nil {
			service.Error(err)
		}
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			return nil
		}
	}
}

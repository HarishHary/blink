package sync

import (
	"context"
	"os"
	"time"

	"github.com/harishhary/blink/internal/configuration"
	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
)

type SyncService struct {
	svcctx.ServiceContext
	dispatcherRepo *dispatchers.DispatcherRepository
}

func New(dispatcherRepo *dispatchers.DispatcherRepository) (*SyncService, error) {
	sc := svcctx.New("BLINK-ALERT-DISPATCHER - SYNC")
	if err := configuration.LoadFromEnvironment(&sc); err != nil {
		return nil, err
	}
	sc.Logger = logger.New(sc.Name(), "dev")
	return &SyncService{
		ServiceContext: sc,
		dispatcherRepo: dispatcherRepo,
	}, nil
}

func (service *SyncService) Name() string { return "alert-dispatcher-sync" }

func (service *SyncService) Run(ctx context.Context) errors.Error {
	configDir := os.Getenv("DISPATCHER_CONFIG_DIR")
	for {
		service.Info("loading dispatcher configs from %s", configDir)
		if err := service.dispatcherRepo.LoadDispatchers(configDir); err != nil {
			service.Error(err)
		}
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			return nil
		}
	}
}

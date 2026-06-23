package services

import (
	"context"

	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/manager"
)

type ConfigSyncService struct {
	svcctx.ServiceContext
	serviceName string
	manager     manager.Manager
}

func NewConfigSyncService(name string, displayName string, manager manager.Manager) *ConfigSyncService {
	sc := svcctx.New(displayName)
	sc.Logger = logger.New(sc.Name(), "dev")
	return &ConfigSyncService{
		ServiceContext: sc,
		serviceName:    name,
		manager:        manager,
	}
}

func (s *ConfigSyncService) Name() string { return s.serviceName }

func (s *ConfigSyncService) Run(ctx context.Context) errors.Error {
	if err := s.manager.Start(ctx); err != nil {
		return errors.NewE(err)
	}
	<-ctx.Done()
	return nil
}

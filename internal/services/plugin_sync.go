package services

import (
	"context"

	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"

	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/manager"
)

type PluginSyncService struct {
	svcctx.ServiceContext
	serviceName string
	manager     manager.Manager
}

// NewPluginSyncService creates a service that starts the plugin manager and waits for
// context cancellation. mgr is the pre-built plugin manager to run.
func NewPluginSyncService(name string, displayName string, manager manager.Manager) (*PluginSyncService, error) {
	sc := svcctx.New(displayName)
	if err := configuration.LoadFromEnvironment(&sc); err != nil {
		return nil, err
	}
	sc.Logger = logger.New(sc.Name(), "dev")

	return &PluginSyncService{
		ServiceContext: sc,
		serviceName:    name,
		manager:        manager,
	}, nil
}

// Name returns the service name.
func (s *PluginSyncService) Name() string { return s.serviceName }

// Run starts the plugin manager (if any) and blocks until ctx is cancelled.
func (s *PluginSyncService) Run(ctx context.Context) errors.Error {
	if err := s.manager.Start(ctx); err != nil {
		s.ErrorF("plugin manager start error: %v", err)
	}
	<-ctx.Done()
	return nil
}

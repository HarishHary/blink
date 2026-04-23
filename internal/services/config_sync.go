package services

import (
	"context"

	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/manager"
)

// ConfigSyncService is the non-generic service wrapper for a ConfigManager,
// mirroring how PluginSyncService wraps PluginManager. It implements services.Service
// so it can be registered alongside other services in the Runner.
type ConfigSyncService struct {
	svcctx.ServiceContext
	serviceName string
	manager     manager.Manager
}

// NewConfigSyncService creates a ConfigSyncService. name is the service name returned
// by Name(); displayName is used for the service context (logging).
func NewConfigSyncService(name, displayName string, manager manager.Manager) *ConfigSyncService {
	sc := svcctx.New(displayName)
	sc.Logger = logger.New(sc.Name(), "dev")
	return &ConfigSyncService{
		ServiceContext: sc,
		serviceName:    name,
		manager:        manager,
	}
}

// Name returns the service name.
func (s *ConfigSyncService) Name() string { return s.serviceName }

// Run starts the config manager (initial reconcile + fsnotify watch loop) and blocks
// until ctx is cancelled. Mirrors PluginSyncService.Run.
func (s *ConfigSyncService) Run(ctx context.Context) errors.Error {
	if err := s.manager.Start(ctx); err != nil {
		s.ErrorF("config manager start error: %v", err)
	}
	<-ctx.Done()
	return nil
}

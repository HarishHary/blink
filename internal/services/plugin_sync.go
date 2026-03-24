package services

import (
	"context"
	"os"

	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"

	"github.com/harishhary/blink/internal/configuration"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

type PluginSyncService struct {
	svcctx.ServiceContext
	serviceName string
	plugin      plugin.Plugin
}

// NewPluginSyncService creates a service that starts the plugin manager and waits for
// context cancellation. newPluginManager is a closure that captures the pool's Sync
// callback so lifecycle events flow directly to the pool with no intermediate bus.
func NewPluginSyncService(
	name, displayName, envVar string,
	newPluginManager func(*logger.Logger, string) plugin.Plugin,
) (*PluginSyncService, error) {
	sc := svcctx.New(displayName)
	if err := configuration.LoadFromEnvironment(&sc); err != nil {
		return nil, err
	}
	sc.Logger = logger.New(sc.Name(), "dev")

	return &PluginSyncService{
		ServiceContext: sc,
		serviceName:    name,
		plugin:         newPluginManager(sc.Logger, os.Getenv(envVar)),
	}, nil
}

// Name returns the service name.
func (s *PluginSyncService) Name() string { return s.serviceName }

// Run starts the plugin manager (if any) and blocks until ctx is cancelled.
func (s *PluginSyncService) Run(ctx context.Context) errors.Error {
	if err := s.plugin.Start(ctx); err != nil {
		s.ErrorF("plugin start error: %v", err)
	}
	<-ctx.Done()
	return nil
}

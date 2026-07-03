package services

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
)

// MaxPluginAttempts is the number of DLQ round-trips an alert makes when a referenced
// plugin is missing before the stage passes the alert through without that plugin.
// This prevents infinite DLQ loops while still retrying transient gaps.
const MaxPluginAttempts = 3

type service interface {
	Name() string
	Run(ctx context.Context) errors.Error
}

// manager is a start-and-detach unit: Start kicks off its work (spawning goroutines) and
// returns, reporting only a fatal bootstrap error. ManagedService adapts it into a service.
type manager interface {
	Start(ctx context.Context) error
}

// ManagedService adapts a manager (start-and-detach) into a service (blocks until ctx done)
// so the Runner can supervise it. Start spawns the manager's work and returns; the <-ctx.Done()
// park is what keeps Run alive until shutdown.
type ManagedService struct {
	name string
	mgr  manager
}

func NewManagedService(name string, mgr manager) *ManagedService {
	return &ManagedService{name: name, mgr: mgr}
}

func (s *ManagedService) Name() string { return s.name }

func (s *ManagedService) Run(ctx context.Context) errors.Error {
	if err := s.mgr.Start(ctx); err != nil {
		return errors.NewE(err)
	}
	<-ctx.Done()
	return nil
}

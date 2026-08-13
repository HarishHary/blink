package controller

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

type ControllerSupervisorLifecycle string

const (
	ControllerSupervisorLifecycleStarting ControllerSupervisorLifecycle = "starting"
	ControllerSupervisorLifecycleRunning  ControllerSupervisorLifecycle = "running"
	ControllerSupervisorLifecycleDraining ControllerSupervisorLifecycle = "draining"
	ControllerSupervisorLifecycleStopping ControllerSupervisorLifecycle = "stopping"
	ControllerSupervisorLifecycleStopped  ControllerSupervisorLifecycle = "stopped"
)

type controllerActorState struct {
	pid    gen.PID
	status ControllerActorStatus
}

type controllerSupervisor[T plugin.Syncable] struct {
	act.Supervisor

	opts ControllerSupervisorOptions[T]

	lifecycle  ControllerSupervisorLifecycle
	controller controllerActorState
}

func NewSupervisor[T plugin.Syncable](opts ControllerSupervisorOptions[T]) gen.ProcessBehavior {
	return &controllerSupervisor[T]{opts: controllerSupervisorOptionsWithDefaults("", opts)}
}

// --- messages ---

type MessageControllerStatusChanged struct {
	status ControllerActorStatus
}

type ControllerSupervisorStopped struct {
	Reason  error
	Drained bool
}

type MessageControllerSupervisorShutdown struct{}

// --- messages ---

func (s *controllerSupervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	if s.opts.ActorOptions.Name == "" {
		return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: actor name is required")
	}
	s.controller.status = ControllerActorStatus{
		Lifecycle:    ControllerActorStarting,
		Availability: runtime.AvailabilityUnavailable,
		Scanner:      ArtifactScannerStatus{Lifecycle: ArtifactScannerStarting, Availability: runtime.AvailabilityUnavailable},
		Publisher:    SnapshotPublisherStatus{Lifecycle: SnapshotPublisherStarting, Availability: runtime.AvailabilityUnavailable},
	}
	s.lifecycle = ControllerSupervisorLifecycleStarting
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy: act.SupervisorStrategyTemporary,
		},
		Children: []act.SupervisorChildSpec{{
			Name: s.opts.ActorOptions.Name,
			Factory: func() gen.ProcessBehavior {
				return NewActor(s.opts.ActorOptions)
			},
		}},
	}, nil
}

func (s *controllerSupervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("controller supervisor: unsupported call %T", request), nil
}

func (s *controllerSupervisor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageControllerSupervisorShutdown:
		if s.lifecycle != ControllerSupervisorLifecycleRunning {
			return nil
		}
		return s.beginDrain()
	case MessageControllerStatusChanged:
		if s.controller.pid != from {
			return nil
		}
		s.controller.status = m.status
		if s.lifecycle == ControllerSupervisorLifecycleDraining {
			return s.advanceShutdown()
		}
	}
	return nil
}

func (s *controllerSupervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	if name != s.opts.ActorOptions.Name || s.controller.pid != (gen.PID{}) {
		return nil
	}
	s.controller.pid = pid
	if s.lifecycle == ControllerSupervisorLifecycleDraining {
		if err := s.Send(pid, MessageControllerActivate{}); err != nil && !stalePIDSendFailure(err) {
			return fmt.Errorf("activate draining controller %s: %w", pid, err)
		}
		return s.sendDrain()
	}
	if s.lifecycle == ControllerSupervisorLifecycleStopping || s.lifecycle == ControllerSupervisorLifecycleStopped {
		return s.sendStop()
	}
	if err := s.Send(pid, MessageControllerActivate{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("activate controller %s: %w", pid, err)
	}
	s.lifecycle = ControllerSupervisorLifecycleRunning
	return nil
}

func (s *controllerSupervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name != s.opts.ActorOptions.Name || s.controller.pid != pid {
		return nil
	}
	s.controller.pid = gen.PID{}
	if s.lifecycle == ControllerSupervisorLifecycleStopping && reason == gen.TerminateReasonNormal {
		return s.advanceShutdown()
	}
	return fmt.Errorf("controller supervisor: child %s (%s) exited unexpectedly: %w", name, pid, reason)
}

func (s *controllerSupervisor[T]) Terminate(reason error) {
	drained := s.lifecycle == ControllerSupervisorLifecycleStopping && s.controller.pid == (gen.PID{})
	s.lifecycle = ControllerSupervisorLifecycleStopped
	stopped := ControllerSupervisorStopped{Reason: reason, Drained: drained}
	if s.opts.onStopped != nil {
		s.opts.onStopped(stopped)
	}
}

func (s *controllerSupervisor[T]) beginDrain() error {
	if s.lifecycle != ControllerSupervisorLifecycleRunning {
		return fmt.Errorf("controller supervisor: cannot drain while %s", s.lifecycle)
	}
	s.lifecycle = ControllerSupervisorLifecycleDraining
	if err := s.sendDrain(); err != nil {
		return err
	}
	return s.advanceShutdown()
}

func (s *controllerSupervisor[T]) advanceShutdown() error {
	if s.lifecycle == ControllerSupervisorLifecycleDraining {
		if s.controller.pid != (gen.PID{}) && s.controller.status.Lifecycle != ControllerActorDrained {
			return nil
		}
		s.lifecycle = ControllerSupervisorLifecycleStopping
		if err := s.sendStop(); err != nil {
			return err
		}
	}
	if s.lifecycle == ControllerSupervisorLifecycleStopping && s.controller.pid == (gen.PID{}) {
		return gen.TerminateReasonNormal
	}
	return nil
}

func (s *controllerSupervisor[T]) sendDrain() error {
	if s.controller.pid == (gen.PID{}) {
		return nil
	}
	if err := s.Send(s.controller.pid, plugin.MessageDrain{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("drain controller %s: %w", s.controller.pid, err)
	}
	return nil
}

func (s *controllerSupervisor[T]) sendStop() error {
	if s.controller.pid == (gen.PID{}) {
		return nil
	}
	if err := s.Send(s.controller.pid, plugin.MessageStop{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("stop controller %s: %w", s.controller.pid, err)
	}
	return nil
}

func stalePIDSendFailure(err error) bool {
	return errors.Is(err, gen.ErrProcessUnknown) || errors.Is(err, gen.ErrProcessTerminated)
}

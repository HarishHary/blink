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
	pid       gen.PID
	status    ControllerActorStatus
	activated bool
}

type controllerSupervisor[T plugin.Syncable] struct {
	act.Supervisor
	opts            ControllerSupervisorOptions[T]
	lifecycle       ControllerSupervisorLifecycle
	controller      controllerActorState
	publisherFences map[uint64]gen.PID
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
	s.publisherFences = make(map[uint64]gen.PID)
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy: act.SupervisorStrategyTransient,
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
	case MessageSnapshotPublisherIOStarted:
		if from != s.controller.pid {
			return nil
		}
		if s.publisherFences == nil {
			s.publisherFences = make(map[uint64]gen.PID)
		}
		s.publisherFences[m.Incarnation] = from
	case MessageSnapshotPublisherIOStopped:
		owner, ok := s.publisherFences[m.Incarnation]
		if !ok || owner != from {
			return nil
		}
		delete(s.publisherFences, m.Incarnation)
		if s.controller.pid == from {
			if err := s.Send(from, m); err != nil && !stalePIDSendFailure(err) {
				return fmt.Errorf("forward snapshot publisher I/O completion to %s: %w", from, err)
			}
		}
		return s.advanceController()
	}
	return nil
}

func (s *controllerSupervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	if name != s.opts.ActorOptions.Name || s.controller.pid != (gen.PID{}) {
		return nil
	}
	s.controller = controllerActorState{
		pid: pid,
		status: ControllerActorStatus{
			Lifecycle:    ControllerActorStarting,
			Availability: runtime.AvailabilityUnavailable,
			Scanner:      ArtifactScannerStatus{Lifecycle: ArtifactScannerStarting, Availability: runtime.AvailabilityUnavailable},
			Publisher:    SnapshotPublisherStatus{Lifecycle: SnapshotPublisherStarting, Availability: runtime.AvailabilityUnavailable},
		},
	}
	return s.advanceController()
}

func (s *controllerSupervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name != s.opts.ActorOptions.Name || s.controller.pid != pid {
		return nil
	}
	s.controller.pid = gen.PID{}
	if reason == gen.TerminateReasonNormal || reason == gen.TerminateReasonShutdown {
		switch s.lifecycle {
		case ControllerSupervisorLifecycleDraining, ControllerSupervisorLifecycleStopping:
			return s.advanceShutdown()
		default:
			return fmt.Errorf("controller supervisor: child %s (%s) exited unexpectedly: %w", name, pid, reason)
		}
	}
	return nil
}

func (s *controllerSupervisor[T]) Terminate(reason error) {
	drained := s.lifecycle == ControllerSupervisorLifecycleStopping && s.controller.pid == (gen.PID{}) && len(s.publisherFences) == 0
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
		if len(s.publisherFences) != 0 {
			return nil
		}
		s.lifecycle = ControllerSupervisorLifecycleStopping
		if err := s.sendStop(); err != nil {
			return err
		}
	}
	if s.lifecycle == ControllerSupervisorLifecycleStopping && s.controller.pid == (gen.PID{}) && len(s.publisherFences) == 0 {
		return gen.TerminateReasonNormal
	}
	return nil
}

func (s *controllerSupervisor[T]) advanceController() error {
	if s.controller.pid == (gen.PID{}) {
		return s.advanceShutdown()
	}
	if s.lifecycle == ControllerSupervisorLifecycleStopping || s.lifecycle == ControllerSupervisorLifecycleStopped {
		return s.sendStop()
	}
	if s.lifecycle == ControllerSupervisorLifecycleDraining {
		if len(s.publisherFences) != 0 {
			return nil
		}
		s.lifecycle = ControllerSupervisorLifecycleStopping
		return s.sendStop()
	}
	if len(s.publisherFences) != 0 || s.controller.activated {
		return nil
	}
	if err := s.Send(s.controller.pid, MessageControllerActivate{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("activate controller %s: %w", s.controller.pid, err)
	}
	s.controller.activated = true
	s.lifecycle = ControllerSupervisorLifecycleRunning
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

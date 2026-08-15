package controller

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

const (
	controllerActorRestartIntensity uint16 = 5
	controllerActorRestartPeriod    uint16 = 10
)

type ControllerSupervisorLifecycle string

const (
	ControllerSupervisorLifecycleStarting ControllerSupervisorLifecycle = "starting"
	ControllerSupervisorLifecycleRunning  ControllerSupervisorLifecycle = "running"
	ControllerSupervisorLifecycleDraining ControllerSupervisorLifecycle = "draining"
	ControllerSupervisorLifecycleStopping ControllerSupervisorLifecycle = "stopping"
)

type controllerActorState struct {
	pid            gen.PID
	status         ControllerActorStatus
	activationSent bool
}

type controllerSupervisor[T plugin.Syncable] struct {
	act.Supervisor
	opts            ControllerSupervisorOptions[T]
	database        backends.Database
	writer          brokers.Writer
	barrier         *publisherIOBarrier
	lifecycle       ControllerSupervisorLifecycle
	controllerActor controllerActorState
	publisherFences map[gen.Alias]gen.PID
}

// newSupervisor constructs the controller supervisor with normalized options.
func newSupervisor[T plugin.Syncable](opts ControllerSupervisorOptions[T], database backends.Database, writer brokers.Writer, barrier *publisherIOBarrier) gen.ProcessBehavior {
	return &controllerSupervisor[T]{opts: controllerSupervisorOptionsWithDefaults("", opts), database: database, writer: writer, barrier: barrier}
}

// --- messages ---

type MessageControllerStatusChanged struct {
	status ControllerActorStatus
}

type MessageControllerSupervisorShutdown struct{}

// --- messages ---

// Init configures the supervised controller actor.
func (s *controllerSupervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	if s.opts.ActorOptions.Name == "" {
		return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: actor name is required")
	}
	s.controllerActor.status = ControllerActorStatus{
		Lifecycle:    ControllerActorStarting,
		Availability: runtime.AvailabilityUnavailable,
		Scanner:      ArtifactScannerStatus{Lifecycle: ArtifactScannerStarting, Availability: runtime.AvailabilityUnavailable},
		Publisher:    SnapshotPublisherStatus{Lifecycle: SnapshotPublisherStarting, Availability: runtime.AvailabilityUnavailable},
	}
	s.lifecycle = ControllerSupervisorLifecycleStarting
	s.publisherFences = make(map[gen.Alias]gen.PID)
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyTransient,
			Intensity: controllerActorRestartIntensity,
			Period:    controllerActorRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{{
			Name: s.opts.ActorOptions.Name,
			Factory: func() gen.ProcessBehavior {
				return newActor(s.opts.ActorOptions, s.database, s.writer, s.barrier)
			},
		}},
	}, nil
}

// HandleMessage coordinates controller lifecycle and publisher I/O fences.
func (s *controllerSupervisor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageControllerSupervisorShutdown:
		if s.lifecycle != ControllerSupervisorLifecycleRunning {
			return nil
		}
		s.lifecycle = ControllerSupervisorLifecycleDraining
		if err := s.sendDrain(); err != nil {
			return err
		}
		return s.advanceShutdown()
	case MessageControllerStatusChanged:
		if s.controllerActor.pid != from {
			return nil
		}
		s.controllerActor.status = m.status
		if s.lifecycle == ControllerSupervisorLifecycleDraining {
			return s.advanceShutdown()
		}
	case MessageSnapshotPublisherIOStarted:
		if s.controllerActor.pid != from {
			return nil
		}
		if s.publisherFences == nil {
			s.publisherFences = make(map[gen.Alias]gen.PID)
		}
		s.publisherFences[m.Alias] = from
	case MessageSnapshotPublisherIOStopped:
		owner, ok := s.publisherFences[m.Alias]
		if !ok || owner != from {
			return nil
		}
		delete(s.publisherFences, m.Alias)
		if s.controllerActor.pid == from {
			if err := s.Send(from, m); err != nil && !stalePIDSendFailure(err) {
				return fmt.Errorf("forward snapshot publisher I/O completion to %s: %w", from, err)
			}
		}
		return s.reconcileController()
	}
	return nil
}

// HandleChildStart tracks a newly started controller actor.
func (s *controllerSupervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	if name != s.opts.ActorOptions.Name || s.controllerActor.pid != (gen.PID{}) {
		return nil
	}
	s.controllerActor = controllerActorState{
		pid: pid,
		status: ControllerActorStatus{
			Lifecycle:    ControllerActorStarting,
			Availability: runtime.AvailabilityUnavailable,
			Scanner: ArtifactScannerStatus{
				Lifecycle:    ArtifactScannerStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
			Publisher: SnapshotPublisherStatus{
				Lifecycle:    SnapshotPublisherStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
		},
	}
	return s.reconcileController()
}

// HandleChildTerminate handles the tracked controller actor exiting.
func (s *controllerSupervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name != s.opts.ActorOptions.Name || s.controllerActor.pid != pid {
		return nil
	}
	s.controllerActor.pid = gen.PID{}
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

// advanceShutdown stops the actor after draining publisher I/O.
func (s *controllerSupervisor[T]) advanceShutdown() error {
	if s.lifecycle == ControllerSupervisorLifecycleDraining {
		if s.controllerActor.pid != (gen.PID{}) && s.controllerActor.status.Lifecycle != ControllerActorDrained {
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
	if s.lifecycle == ControllerSupervisorLifecycleStopping && s.controllerActor.pid == (gen.PID{}) && len(s.publisherFences) == 0 {
		return gen.TerminateReasonNormal
	}
	return nil
}

// reconcileController activates or stops the current controller actor.
func (s *controllerSupervisor[T]) reconcileController() error {
	if s.controllerActor.pid == (gen.PID{}) {
		return s.advanceShutdown()
	}
	if s.lifecycle == ControllerSupervisorLifecycleStopping {
		return s.sendStop()
	}
	if s.lifecycle == ControllerSupervisorLifecycleDraining {
		if len(s.publisherFences) != 0 {
			return nil
		}
		s.lifecycle = ControllerSupervisorLifecycleStopping
		return s.sendStop()
	}
	if len(s.publisherFences) != 0 || s.controllerActor.activationSent {
		return nil
	}
	if err := s.Send(s.controllerActor.pid, MessageControllerActivate{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("activate controller %s: %w", s.controllerActor.pid, err)
	}
	s.controllerActor.activationSent = true
	s.lifecycle = ControllerSupervisorLifecycleRunning
	return nil
}

// sendDrain asks the controller actor to drain.
func (s *controllerSupervisor[T]) sendDrain() error {
	if s.controllerActor.pid == (gen.PID{}) {
		return nil
	}
	if err := s.Send(s.controllerActor.pid, plugin.MessageDrain{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("drain controller %s: %w", s.controllerActor.pid, err)
	}
	return nil
}

// sendStop asks the controller actor to stop.
func (s *controllerSupervisor[T]) sendStop() error {
	if s.controllerActor.pid == (gen.PID{}) {
		return nil
	}
	if err := s.Send(s.controllerActor.pid, plugin.MessageStop{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("stop controller %s: %w", s.controllerActor.pid, err)
	}
	return nil
}

// stalePIDSendFailure reports sends lost to an exiting process.
func stalePIDSendFailure(err error) bool {
	return errors.Is(err, gen.ErrProcessUnknown) || errors.Is(err, gen.ErrProcessTerminated)
}

// HandleCall rejects unsupported supervisor calls.
func (s *controllerSupervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("controller supervisor: unsupported call %T", request), nil
}

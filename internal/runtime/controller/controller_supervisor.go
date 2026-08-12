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

type SupervisorChild struct {
	name    gen.Atom
	factory gen.ProcessFactory
}

func NewSupervisorChild[T plugin.Syncable](name gen.Atom, opts Options[T]) SupervisorChild {
	return SupervisorChild{name: name, factory: func() gen.ProcessBehavior { return NewActor(opts) }}
}

type controllerActorState struct {
	pid    gen.PID
	status ControllerActorStatus
}

type ControllerSupervisorOptions struct {
	Name     gen.Atom
	Children []SupervisorChild
	Stopped  chan<- ControllerSupervisorStopped
}

type controllerSupervisor struct {
	act.Supervisor

	opts ControllerSupervisorOptions

	lifecycle   ControllerSupervisorLifecycle
	controllers map[gen.Atom]*controllerActorState
}

func NewSupervisor(opts ControllerSupervisorOptions) gen.ProcessBehavior {
	if opts.Name == "" {
		opts.Name = "controller-supervisor"
	}
	return &controllerSupervisor{opts: opts}
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

func (s *controllerSupervisor) Init(...any) (act.SupervisorSpec, error) {
	if len(s.opts.Children) == 0 {
		return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: children are required")
	}
	children := make([]act.SupervisorChildSpec, 0, len(s.opts.Children))
	s.controllers = make(map[gen.Atom]*controllerActorState, len(s.opts.Children))
	for _, child := range s.opts.Children {
		if child.name == "" || child.factory == nil {
			return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: child name and factory are required")
		}
		if _, exists := s.controllers[child.name]; exists {
			return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: duplicate child %q", child.name)
		}
		s.controllers[child.name] = &controllerActorState{status: ControllerActorStatus{
			Lifecycle:    ControllerActorStarting,
			Availability: runtime.AvailabilityUnavailable,
			Scanner:      ArtifactScannerStatus{Lifecycle: ArtifactScannerStarting, Availability: runtime.AvailabilityUnavailable},
			Publisher:    SnapshotPublisherStatus{Lifecycle: SnapshotPublisherStarting, Availability: runtime.AvailabilityUnavailable},
		}}
		children = append(children, act.SupervisorChildSpec{Name: child.name, Factory: child.factory})
	}
	s.lifecycle = ControllerSupervisorLifecycleStarting
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy: act.SupervisorStrategyTemporary,
		},
		Children: children,
	}, nil
}

func (s *controllerSupervisor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("controller supervisor: unsupported call %T", request), nil
}

func (s *controllerSupervisor) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageControllerSupervisorShutdown:
		if s.lifecycle != ControllerSupervisorLifecycleRunning {
			return nil
		}
		return s.beginDrain()
	case MessageControllerStatusChanged:
		for _, child := range s.controllers {
			if child.pid != from {
				continue
			}
			child.status = m.status
			if s.lifecycle == ControllerSupervisorLifecycleDraining {
				return s.advanceShutdown()
			}
			break
		}
	}
	return nil
}

func (s *controllerSupervisor) HandleChildStart(name gen.Atom, pid gen.PID) error {
	child, ok := s.controllers[name]
	if !ok || child.pid != (gen.PID{}) {
		return nil
	}
	child.pid = pid
	if s.lifecycle == ControllerSupervisorLifecycleDraining {
		if err := s.Send(pid, MessageControllerActivate{}); err != nil && !stalePIDSendFailure(err) {
			return fmt.Errorf("activate draining controller %s: %w", pid, err)
		}
		return s.sendDrain(child)
	}
	if s.lifecycle == ControllerSupervisorLifecycleStopping || s.lifecycle == ControllerSupervisorLifecycleStopped {
		return s.sendStop(child)
	}
	if err := s.Send(pid, MessageControllerActivate{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("activate controller %s: %w", pid, err)
	}
	for _, child := range s.controllers {
		if child.pid == (gen.PID{}) {
			return nil
		}
	}
	s.lifecycle = ControllerSupervisorLifecycleRunning
	return nil
}

func (s *controllerSupervisor) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	child, ok := s.controllers[name]
	if !ok || child.pid != pid {
		return nil
	}
	child.pid = gen.PID{}
	if s.lifecycle == ControllerSupervisorLifecycleStopping && reason == gen.TerminateReasonNormal {
		return s.advanceShutdown()
	}
	err := fmt.Errorf("controller supervisor: child %s (%s) exited unexpectedly: %w", name, pid, reason)
	return err
}

func (s *controllerSupervisor) Terminate(reason error) {
	drained := s.lifecycle == ControllerSupervisorLifecycleStopping
	for _, child := range s.controllers {
		if child.pid != (gen.PID{}) {
			drained = false
			break
		}
	}
	s.lifecycle = ControllerSupervisorLifecycleStopped
	if s.opts.Stopped != nil {
		select {
		case s.opts.Stopped <- ControllerSupervisorStopped{Reason: reason, Drained: drained}:
		default:
		}
	}
}

func (s *controllerSupervisor) beginDrain() error {
	if s.lifecycle != ControllerSupervisorLifecycleRunning {
		return fmt.Errorf("controller supervisor: cannot drain while %s", s.lifecycle)
	}
	s.lifecycle = ControllerSupervisorLifecycleDraining
	for _, child := range s.controllers {
		if err := s.sendDrain(child); err != nil {
			return err
		}
	}
	return s.advanceShutdown()
}

func (s *controllerSupervisor) advanceShutdown() error {
	if s.lifecycle == ControllerSupervisorLifecycleDraining {
		for _, child := range s.controllers {
			if child.pid != (gen.PID{}) && child.status.Lifecycle != ControllerActorDrained {
				return nil
			}
		}
		s.lifecycle = ControllerSupervisorLifecycleStopping
		for _, child := range s.controllers {
			if err := s.sendStop(child); err != nil {
				return err
			}
		}
	}
	if s.lifecycle == ControllerSupervisorLifecycleStopping {
		for _, child := range s.controllers {
			if child.pid != (gen.PID{}) {
				return nil
			}
		}
		return gen.TerminateReasonNormal
	}
	return nil
}

func (s *controllerSupervisor) sendDrain(child *controllerActorState) error {
	if child.pid == (gen.PID{}) {
		return nil
	}
	if err := s.Send(child.pid, plugin.MessageDrain{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("drain controller %s: %w", child.pid, err)
	}
	return nil
}

func (s *controllerSupervisor) sendStop(child *controllerActorState) error {
	if child.pid == (gen.PID{}) {
		return nil
	}
	if err := s.Send(child.pid, plugin.MessageStop{}); err != nil && !stalePIDSendFailure(err) {
		return fmt.Errorf("stop controller %s: %w", child.pid, err)
	}
	return nil
}

func stalePIDSendFailure(err error) bool {
	return errors.Is(err, gen.ErrProcessUnknown) || errors.Is(err, gen.ErrProcessTerminated)
}

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
	actorRestartIntensity uint16 = 5
	actorRestartPeriod    uint16 = 10
)

type SupervisorLifecycle string

const (
	SupervisorStarting SupervisorLifecycle = "starting"
	SupervisorRunning  SupervisorLifecycle = "running"
	SupervisorDraining SupervisorLifecycle = "draining"
	SupervisorStopping SupervisorLifecycle = "stopping"
)

type actorState struct {
	pid            gen.PID
	status         ActorStatus
	activationSent bool
}

type supervisor[T plugin.Syncable] struct {
	act.Supervisor
	opts            SupervisorOptions[T]
	database        backends.Database
	writer          brokers.Writer
	barrier         *publisherIOBarrier
	lifecycle       SupervisorLifecycle
	actor           actorState
	publisherFences map[gen.Alias]gen.PID
}

// newSupervisor constructs the controller supervisor with normalized options.
func newSupervisor[T plugin.Syncable](opts SupervisorOptions[T], database backends.Database, writer brokers.Writer, barrier *publisherIOBarrier) gen.ProcessBehavior {
	return &supervisor[T]{opts: supervisorOptionsWithDefaults("", opts), database: database, writer: writer, barrier: barrier}
}

// --- messages ---

type MessageActorStatusChanged struct {
	status ActorStatus
}

// --- messages ---

// Init configures the supervised controller actor.
func (s *supervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	if s.opts.ActorOptions.Name == "" {
		return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: actor name is required")
	}
	s.actor.status = ActorStatus{
		Lifecycle:    ActorStarting,
		Availability: runtime.AvailabilityUnavailable,
		Scanner:      ArtifactScannerMetaStatus{Lifecycle: ArtifactScannerMetaStarting, Availability: runtime.AvailabilityUnavailable},
		Publisher:    SnapshotPublisherMetaStatus{Lifecycle: SnapshotPublisherMetaStarting, Availability: runtime.AvailabilityUnavailable},
	}
	s.lifecycle = SupervisorStarting
	s.publisherFences = make(map[gen.Alias]gen.PID)
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyTransient,
			Intensity: actorRestartIntensity,
			Period:    actorRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{{
			Name: s.opts.ActorOptions.Name,
			Options: gen.ProcessOptions{
				PreserveMailbox: true,
			},
			Factory: func() gen.ProcessBehavior {
				return newActor(s.opts.ActorOptions, s.database, s.writer, s.barrier)
			},
		}},
	}, nil
}

// HandleMessage coordinates controller lifecycle and publisher I/O fences.
func (s *supervisor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case plugin.MessageStop:
		if s.lifecycle != SupervisorRunning {
			return nil
		}
		s.lifecycle = SupervisorDraining
		s.Log().Info("controller supervisor draining: name=%s child=%s publisher_fences=%d", s.opts.Name, s.actor.pid, len(s.publisherFences))
		if err := s.sendDrain(); err != nil {
			return err
		}
		return s.advanceShutdown()
	case MessageActorStatusChanged:
		if s.actor.pid != from {
			return nil
		}
		previous := s.actor.status.Lifecycle
		s.actor.status = m.status
		if previous != m.status.Lifecycle {
			s.Log().Debug("controller child lifecycle changed: name=%s child=%s lifecycle=%s", s.opts.Name, from, m.status.Lifecycle)
		}
		if s.lifecycle == SupervisorDraining {
			return s.advanceShutdown()
		}
	case MessageSnapshotPublisherIOStarted:
		if s.actor.pid != from {
			return nil
		}
		if s.publisherFences == nil {
			s.publisherFences = make(map[gen.Alias]gen.PID)
		}
		s.publisherFences[m.Alias] = from
		s.Log().Debug("snapshot publisher I/O fence registered: name=%s child=%s alias=%s active=%d", s.opts.Name, from, m.Alias, len(s.publisherFences))
	case MessageSnapshotPublisherIOStopped:
		owner, ok := s.publisherFences[m.Alias]
		if !ok || owner != from {
			return nil
		}
		delete(s.publisherFences, m.Alias)
		s.Log().Debug("snapshot publisher I/O fence released: name=%s child=%s alias=%s active=%d", s.opts.Name, from, m.Alias, len(s.publisherFences))
		if s.actor.pid == from {
			if err := s.Send(from, m); err != nil && !stalePIDSendFailure(err) {
				s.Log().Error("snapshot publisher I/O completion forwarding failed: name=%s child=%s alias=%s error=%v", s.opts.Name, from, m.Alias, err)
				return fmt.Errorf("forward snapshot publisher I/O completion to %s: %w", from, err)
			}
		}
		return s.reconcileActor()
	}
	return nil
}

// HandleChildStart tracks a newly started controller actor.
func (s *supervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	if name != s.opts.ActorOptions.Name || s.actor.pid != (gen.PID{}) {
		return nil
	}
	s.actor = actorState{
		pid: pid,
		status: ActorStatus{
			Lifecycle:    ActorStarting,
			Availability: runtime.AvailabilityUnavailable,
			Scanner: ArtifactScannerMetaStatus{
				Lifecycle:    ArtifactScannerMetaStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
			Publisher: SnapshotPublisherMetaStatus{
				Lifecycle:    SnapshotPublisherMetaStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
		},
	}
	s.Log().Info("controller child started: name=%s child=%s", s.opts.Name, pid)
	return s.reconcileActor()
}

// HandleChildTerminate handles the tracked controller actor exiting.
func (s *supervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name != s.opts.ActorOptions.Name || s.actor.pid != pid {
		return nil
	}
	s.actor.pid = gen.PID{}
	if reason == gen.TerminateReasonNormal || reason == gen.TerminateReasonShutdown {
		switch s.lifecycle {
		case SupervisorDraining, SupervisorStopping:
			s.Log().Info("controller child stopped: name=%s child=%s reason=%v", s.opts.Name, pid, reason)
			return s.advanceShutdown()
		default:
			s.Log().Error("controller child stopped unexpectedly: name=%s child=%s reason=%v", s.opts.Name, pid, reason)
			return fmt.Errorf("controller supervisor: child %s (%s) exited unexpectedly: %w", name, pid, reason)
		}
	}
	s.Log().Error("controller child failed: name=%s child=%s reason=%v", s.opts.Name, pid, reason)
	return nil
}

// advanceShutdown stops the actor after draining publisher I/O.
func (s *supervisor[T]) advanceShutdown() error {
	if s.lifecycle == SupervisorDraining {
		if s.actor.pid != (gen.PID{}) && s.actor.status.Lifecycle != ActorDrained {
			return nil
		}
		if len(s.publisherFences) != 0 {
			return nil
		}
		s.lifecycle = SupervisorStopping
		s.Log().Info("controller supervisor stopping: name=%s", s.opts.Name)
		if err := s.sendStop(); err != nil {
			return err
		}
	}
	if s.lifecycle == SupervisorStopping && s.actor.pid == (gen.PID{}) && len(s.publisherFences) == 0 {
		s.Log().Info("controller supervisor stopped: name=%s", s.opts.Name)
		return gen.TerminateReasonNormal
	}
	return nil
}

// reconcileActor activates or stops the current controller actor.
func (s *supervisor[T]) reconcileActor() error {
	if s.actor.pid == (gen.PID{}) {
		return s.advanceShutdown()
	}
	if s.lifecycle == SupervisorStopping {
		return s.sendStop()
	}
	if s.lifecycle == SupervisorDraining {
		if len(s.publisherFences) != 0 {
			return nil
		}
		s.lifecycle = SupervisorStopping
		return s.sendStop()
	}
	if len(s.publisherFences) != 0 || s.actor.activationSent {
		if len(s.publisherFences) != 0 {
			s.Log().Debug("controller activation waiting for publisher I/O: name=%s child=%s active=%d", s.opts.Name, s.actor.pid, len(s.publisherFences))
		}
		return nil
	}
	err := s.Send(s.actor.pid, MessageActorActivate{})
	if err != nil && !stalePIDSendFailure(err) {
		s.Log().Error("controller activation failed: name=%s child=%s error=%v", s.opts.Name, s.actor.pid, err)
		return fmt.Errorf("activate controller %s: %w", s.actor.pid, err)
	}
	s.actor.activationSent = true
	s.lifecycle = SupervisorRunning
	if err == nil {
		s.Log().Info("controller activated: name=%s child=%s", s.opts.Name, s.actor.pid)
	}
	return nil
}

// sendDrain asks the controller actor to drain.
func (s *supervisor[T]) sendDrain() error {
	if s.actor.pid == (gen.PID{}) {
		return nil
	}
	err := s.Send(s.actor.pid, plugin.MessageDrain{})
	if err != nil && !stalePIDSendFailure(err) {
		s.Log().Error("controller drain request failed: name=%s child=%s error=%v", s.opts.Name, s.actor.pid, err)
		return fmt.Errorf("drain controller %s: %w", s.actor.pid, err)
	}
	if err == nil {
		s.Log().Debug("controller drain requested: name=%s child=%s", s.opts.Name, s.actor.pid)
	}
	return nil
}

// sendStop asks the controller actor to stop.
func (s *supervisor[T]) sendStop() error {
	if s.actor.pid == (gen.PID{}) {
		return nil
	}
	err := s.Send(s.actor.pid, plugin.MessageStop{})
	if err != nil && !stalePIDSendFailure(err) {
		s.Log().Error("controller stop request failed: name=%s child=%s error=%v", s.opts.Name, s.actor.pid, err)
		return fmt.Errorf("stop controller %s: %w", s.actor.pid, err)
	}
	if err == nil {
		s.Log().Debug("controller stop requested: name=%s child=%s", s.opts.Name, s.actor.pid)
	}
	return nil
}

// stalePIDSendFailure reports sends lost to an exiting process.
func stalePIDSendFailure(err error) bool {
	return errors.Is(err, gen.ErrProcessUnknown) || errors.Is(err, gen.ErrProcessTerminated)
}

// HandleCall rejects unsupported supervisor calls.
func (s *supervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("controller supervisor: unsupported call %T", request), nil
}

package snapshot

import (
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
)

const (
	snapshotReaderActorRestartIntensity uint16 = 5
	snapshotReaderActorRestartPeriod    uint16 = 10
)

// SnapshotReaderEvents identifies the two buffered events produced by a snapshot
// supervisor. Snapshot carries *snapshot.Snapshot; Status carries
// SnapshotReaderActorStatus.
type SnapshotReaderEvents struct {
	Snapshot      gen.Event
	Status        gen.Event
	snapshotToken gen.Ref
	statusToken   gen.Ref
}

// EventsFor returns the stable event identifiers for a snapshot subtree.
func EventsFor(node gen.Node, name gen.Atom) SnapshotReaderEvents {
	return SnapshotReaderEvents{
		Snapshot: gen.Event{Name: gen.Atom(string(name) + "-snapshot"), Node: node.Name()},
		Status:   gen.Event{Name: gen.Atom(string(name) + "-status"), Node: node.Name()},
	}
}

// SnapshotReaderSupervisorOptions configures the reader child of a snapshot supervisor.
type SnapshotReaderSupervisorOptions struct {
	Name          gen.Atom
	Logger        *logger.Logger
	ReaderFactory func() brokers.Reader
	RestartMin    time.Duration
	RestartMax    time.Duration
}

// SnapshotSupervisorOptions configures a raw reader and its typed projection sibling.
type SnapshotSupervisorOptions[T any] struct {
	SnapshotReaderSupervisorOptions
	Projection  ProjectionSpec[T]
	Coordinator gen.ProcessID
	Stopped     chan<- error
}

// SnapshotSupervisor owns a reader followed by a typed projection actor.
// Rest-for-one restarts the projection whenever its reader restarts.
type SnapshotSupervisor[T any] struct {
	act.Supervisor

	opts SnapshotSupervisorOptions[T]

	reader     snapshotReaderActorState
	projection projectionActorState
	events     SnapshotReaderEvents
}

type snapshotReaderActorState struct {
	pid    gen.PID
	status SnapshotReaderActorStatus
}

type projectionActorState struct {
	pid         gen.PID
	epoch       uint64
	activations map[uint64]projectionActivation
	status      ProjectionActorStatus
}

type projectionActivation struct {
	runtimePID    gen.PID
	projectionPID gen.PID
	generation    int64
	epoch         uint64
}

// NewSnapshotSupervisor creates a reader/projection supervisor with stable child names.
func NewSnapshotSupervisor[T any](opts SnapshotSupervisorOptions[T]) *SnapshotSupervisor[T] {
	return &SnapshotSupervisor[T]{opts: SnapshotSupervisorOptions[T]{
		SnapshotReaderSupervisorOptions: defaultOptions(opts.SnapshotReaderSupervisorOptions),
		Projection:                      opts.Projection,
		Coordinator:                     opts.Coordinator,
		Stopped:                         opts.Stopped,
	}}
}

// --- messages ---

// The supervisor accepts status only from its current reader child PID.
type MessageSnapshotReaderStatusChanged struct{ status SnapshotReaderActorStatus }

// --- messages ---

func (s *SnapshotSupervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	if s.opts.Name == "" || s.opts.Logger == nil || s.opts.ReaderFactory == nil {
		return act.SupervisorSpec{}, fmt.Errorf("actor snapshot: name, logger, and reader factory are required")
	}
	if s.opts.Projection.Parse == nil || s.opts.Projection.Clone == nil || s.opts.Projection.MaxProcs == nil {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: parse, clone, and max procs are required")
	}
	if s.opts.Projection.CommitMode != ProjectionCommitDirect && s.opts.Projection.CommitMode != ProjectionCommitExternal {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: invalid commit mode")
	}
	if s.opts.Projection.CommitMode == ProjectionCommitExternal && s.opts.Coordinator == (gen.ProcessID{}) {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: coordinator is required for external mode")
	}
	if err := s.RegisterName(s.opts.Name); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot supervisor %q: %w", s.opts.Name, err)
	}

	s.events = EventsFor(s.Node(), s.opts.Name)
	s.projection.activations = make(map[uint64]projectionActivation)
	// Keep the two in-flight external generations available to a restarted projection.
	snapshotToken, err := s.RegisterEvent(s.events.Snapshot.Name, gen.EventOptions{Buffer: maxStagedProjectionGenerations})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot event: %w", err)
	}
	s.events.snapshotToken = snapshotToken
	statusToken, err := s.RegisterEvent(s.events.Status.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot status event: %w", err)
	}
	s.events.statusToken = statusToken
	s.reader.status = newSnapshotReaderStatus()
	s.projection.status = newProjectionActorStatus()
	s.publishStatus()

	return act.SupervisorSpec{
		Type:                act.SupervisorTypeRestForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyPermanent,
			Intensity: snapshotReaderActorRestartIntensity,
			Period:    snapshotReaderActorRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{
			{
				Name: s.readerActorName(),
				Factory: func() gen.ProcessBehavior {
					return &snapshotReaderActor{opts: s.opts.SnapshotReaderSupervisorOptions}
				},
			},
			{
				Name: projectionActorName(s.opts.Name),
				Factory: func() gen.ProcessBehavior {
					return &snapshotProjectionActor[T]{events: s.events, spec: s.opts.Projection}
				},
			},
		},
	}, nil
}

func (s *SnapshotSupervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	if name == projectionActorName(s.opts.Name) {
		s.projection.pid = pid
		s.projection.epoch++
		s.projection.status = newProjectionActorStatus()
		return nil
	}
	if name != s.readerActorName() {
		return nil
	}
	s.reader.pid = pid
	s.reader.status = newSnapshotReaderStatus()
	s.publishStatus()
	return s.Send(pid, MessageSnapshotReaderActivate{
		snapshotEventName:  s.events.Snapshot.Name,
		snapshotEventToken: s.events.snapshotToken,
	})
}

func (s *SnapshotSupervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name == projectionActorName(s.opts.Name) && s.projection.pid == pid {
		s.projection.pid = gen.PID{}
		s.projection.status.Lifecycle = ProjectionActorRestarting
		s.projection.status.Availability = runtime.AvailabilityUnavailable
		s.projection.status.LastError = reason
		if s.opts.Projection.CommitMode == ProjectionCommitExternal {
			_ = s.Send(s.opts.Coordinator, MessageProjectionUnavailable{ProjectionEpoch: s.projection.epoch})
		}
		for request, activation := range s.projection.activations {
			if activation.projectionPID != pid {
				continue
			}
			delete(s.projection.activations, request)
			_ = s.Send(activation.runtimePID, MessageProjectionActivated{
				Generation:      activation.generation,
				Request:         request,
				ProjectionEpoch: activation.epoch,
				Err:             ErrProjectionNotPrepared,
			})
		}
		return nil
	}
	if name != s.readerActorName() || s.reader.pid != pid {
		return nil
	}
	s.reader.pid = gen.PID{}
	s.reader.status.Lifecycle = SnapshotReaderRestarting
	s.reader.status.Availability = runtime.AvailabilityUnavailable
	s.reader.status.LastError = reason
	s.reader.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.reader.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.reader.status.Reader.CaughtUp = false
	s.reader.status.Reader.RestartPending = false
	s.publishStatus()
	return nil
}

func (s *SnapshotSupervisor[T]) HandleMessage(from gen.PID, message any) error {
	switch message := message.(type) {
	case MessageSnapshotReaderStatusChanged:
		if from == s.reader.pid {
			s.reader.status = message.status
			s.publishStatus()
		}
	case messageProjectionStatusChanged:
		if from == s.projection.pid {
			s.projection.status = message.status
		}
	case MessageProjectionPrepared:
		if from == s.projection.pid && s.opts.Projection.CommitMode == ProjectionCommitExternal {
			message.ProjectionEpoch = s.projection.epoch
			_ = s.Send(s.opts.Coordinator, message)
		}
	case MessageProjectionActivate:
		if !s.authenticatedCoordinator(from) {
			return nil
		}
		if s.projection.pid == (gen.PID{}) {
			_ = s.Send(from, s.notPreparedActivation(message))
			return nil
		}
		if message.ProjectionEpoch != s.projection.epoch {
			_ = s.Send(from, s.notPreparedActivation(message))
			return nil
		}
		s.projection.activations[message.Request] = projectionActivation{runtimePID: from, projectionPID: s.projection.pid, generation: message.Generation, epoch: message.ProjectionEpoch}
		if err := s.Send(s.projection.pid, messageProjectionActivate{generation: message.Generation, request: message.Request, projectionEpoch: message.ProjectionEpoch}); err != nil {
			delete(s.projection.activations, message.Request)
			_ = s.Send(from, MessageProjectionActivated{Generation: message.Generation, Request: message.Request, ProjectionEpoch: message.ProjectionEpoch, Err: err})
		}
	case messageProjectionActivated:
		activation, ok := s.projection.activations[message.request]
		if !ok || from != s.projection.pid || from != activation.projectionPID || message.generation != activation.generation || message.projectionEpoch != activation.epoch {
			return nil
		}
		delete(s.projection.activations, message.request)
		_ = s.Send(activation.runtimePID, MessageProjectionActivated{Generation: message.generation, Request: message.request, ProjectionEpoch: message.projectionEpoch, Err: message.err})
	}
	return nil
}

func (s *SnapshotSupervisor[T]) notPreparedActivation(message MessageProjectionActivate) MessageProjectionActivated {
	return MessageProjectionActivated{
		Generation: message.Generation, Request: message.Request, ProjectionEpoch: s.projection.epoch, Err: ErrProjectionNotPrepared,
	}
}

func (s *SnapshotSupervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot supervisor: unsupported call %T", request), nil
}

func (s *SnapshotSupervisor[T]) authenticatedCoordinator(pid gen.PID) bool {
	if s.opts.Projection.CommitMode != ProjectionCommitExternal || pid.Node != s.opts.Coordinator.Node {
		return false
	}
	info, err := s.Node().ProcessInfo(pid)
	return err == nil && info.Name == s.opts.Coordinator.Name
}

func (s *SnapshotSupervisor[T]) Terminate(reason error) {
	s.projection.status.Lifecycle = ProjectionActorStopped
	s.projection.status.Availability = runtime.AvailabilityUnavailable
	s.projection.status.LastError = reason
	s.reader.status.Lifecycle = SnapshotReaderStopped
	s.reader.status.Availability = runtime.AvailabilityUnavailable
	s.reader.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.reader.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.publishStatus()
	if s.opts.Stopped != nil {
		select {
		case s.opts.Stopped <- reason:
		default:
		}
	}
}

func (s *SnapshotSupervisor[T]) readerActorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-reader")
}

func projectionActorName(name gen.Atom) gen.Atom {
	return gen.Atom(string(name) + "-projection")
}

func (s *SnapshotSupervisor[T]) publishStatus() {
	if s.events.statusToken != (gen.Ref{}) {
		_ = s.SendEvent(s.events.Status.Name, s.events.statusToken, s.reader.status)
	}
}

func newSnapshotReaderStatus() SnapshotReaderActorStatus {
	return SnapshotReaderActorStatus{
		Lifecycle:    SnapshotReaderStarting,
		Availability: runtime.AvailabilityUnavailable,
		Reader: SnapshotReaderMetaStatus{
			Lifecycle:    SnapshotReaderMetaStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
}

func newProjectionActorStatus() ProjectionActorStatus {
	return ProjectionActorStatus{
		Lifecycle:    ProjectionActorStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
}

func defaultOptions(opts SnapshotReaderSupervisorOptions) SnapshotReaderSupervisorOptions {
	if opts.Name == "" {
		opts.Name = "snapshot-reader"
	}
	if opts.RestartMin <= 0 {
		opts.RestartMin = 100 * time.Millisecond
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = 5 * time.Second
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	return opts
}

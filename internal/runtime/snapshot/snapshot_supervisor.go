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

// SnapshotReaderEvents identifies the buffered snapshot and status events produced
// by a snapshot supervisor. Each retains only its latest value. Snapshot carries
// *snapshot.Snapshot; Status carries SnapshotReaderActorStatus.
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

// SnapshotReaderActorOptions configures the reader child of a snapshot supervisor.
type SnapshotReaderActorOptions struct {
	Name          gen.Atom
	Logger        *logger.Logger
	ReaderFactory func() brokers.Reader
	RestartMin    time.Duration
	RestartMax    time.Duration
}

// SnapshotReaderSupervisorOptions configures a raw reader and its typed projection sibling.
type SnapshotReaderSupervisorOptions[T any] struct {
	SnapshotReaderActorOptions
	Projection     ProjectionSpec[T]
	ProjectionMode ProjectionCommitMode
	Stopped        chan<- error
}

// SnapshotSupervisor owns a reader followed by a typed projection actor.
// Rest-for-one restarts the projection whenever its reader restarts.
type SnapshotSupervisor[T any] struct {
	act.Supervisor
	opts            SnapshotReaderSupervisorOptions[T]
	readerActor     snapshotReaderActorState
	projectionActor projectionActorState
	events          SnapshotReaderEvents
}

type snapshotReaderActorState struct {
	pid    gen.PID
	status SnapshotReaderActorStatus
}

type projectionActorState struct {
	pid              gen.PID
	commitGeneration int64
	status           ProjectionActorStatus
}

// NewSupervisor creates a reader/projection supervisor with stable child names.
func NewSupervisor[T any](opts SnapshotReaderSupervisorOptions[T]) *SnapshotSupervisor[T] {
	return &SnapshotSupervisor[T]{opts: SnapshotReaderSupervisorOptions[T]{
		SnapshotReaderActorOptions: defaultOptions(opts.SnapshotReaderActorOptions),
		Projection:                 opts.Projection,
		ProjectionMode:             opts.ProjectionMode,
		Stopped:                    opts.Stopped,
	}}
}

// --- messages ---

// The supervisor accepts status only from its current reader child PID.
type MessageSnapshotReaderStatusChanged struct{ status SnapshotReaderActorStatus }

// --- messages ---

// Init validates options and configures the supervised reader and projection actors.
func (s *SnapshotSupervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	defer s.reportStatus()

	if s.opts.Name == "" || s.opts.Logger == nil || s.opts.ReaderFactory == nil {
		return act.SupervisorSpec{}, fmt.Errorf("actor snapshot: name, logger, and reader factory are required")
	}
	if s.opts.Projection.Parse == nil || s.opts.Projection.Clone == nil || s.opts.Projection.MaxProcs == nil {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: parse, clone, and max procs are required")
	}
	if s.opts.ProjectionMode != ProjectionCommitDirect && s.opts.ProjectionMode != ProjectionCommitExternal {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: invalid commit mode")
	}
	if err := s.RegisterName(s.opts.Name); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot supervisor %q: %w", s.opts.Name, err)
	}

	s.events = EventsFor(s.Node(), s.opts.Name)
	// Keep only the latest snapshot available to a restarted projection.
	snapshotToken, err := s.RegisterEvent(s.events.Snapshot.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot event: %w", err)
	}
	s.events.snapshotToken = snapshotToken
	statusToken, err := s.RegisterEvent(s.events.Status.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot status event: %w", err)
	}
	s.events.statusToken = statusToken
	s.readerActor.status = newSnapshotReaderStatus()
	s.projectionActor.status = newProjectionActorStatus()

	return act.SupervisorSpec{
		Type:                act.SupervisorTypeRestForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyTransient,
			Intensity: snapshotReaderActorRestartIntensity,
			Period:    snapshotReaderActorRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{
			{
				Name: readerActorName(s.opts.Name),
				Factory: func() gen.ProcessBehavior {
					return &snapshotReaderActor{opts: s.opts.SnapshotReaderActorOptions}
				},
			},
			{
				Name: projectionActorName(s.opts.Name),
				Factory: func() gen.ProcessBehavior {
					return &snapshotProjectionActor[T]{events: s.events, spec: s.opts.Projection, mode: s.opts.ProjectionMode}
				},
			},
		},
	}, nil
}

// HandleChildStart tracks and activates a started reader or projection child.
func (s *SnapshotSupervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	defer s.reportStatus()

	if name == projectionActorName(s.opts.Name) {
		s.projectionActor.pid = pid
		s.projectionActor.commitGeneration = 0
		s.projectionActor.status = newProjectionActorStatus()
		// Stale child-start callbacks may race a replacement and fail to send.
		_ = s.Send(pid, MessageProjectionStart{})
		return nil
	}
	if name != readerActorName(s.opts.Name) {
		return nil
	}
	s.readerActor.pid = pid
	s.readerActor.status = newSnapshotReaderStatus()
	return s.Send(pid, MessageSnapshotReaderActivate{
		snapshotEventName:  s.events.Snapshot.Name,
		snapshotEventToken: s.events.snapshotToken,
	})
}

// HandleChildTerminate records a terminated child and reports external commit failures.
func (s *SnapshotSupervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	defer s.reportStatus()

	if name == projectionActorName(s.opts.Name) && s.projectionActor.pid == pid {
		s.projectionActor.pid = gen.PID{}
		s.projectionActor.status.Lifecycle = ProjectionActorRestarting
		s.projectionActor.status.Availability = runtime.AvailabilityUnavailable
		s.projectionActor.status.PreparedGeneration = 0
		if s.opts.ProjectionMode == ProjectionCommitExternal {
			_ = s.Send(s.Parent(), MessageProjectionStatusChanged{Status: s.projectionActor.status, ProjectionPID: pid})
		}
		if generation := s.projectionActor.commitGeneration; generation != 0 {
			s.projectionActor.commitGeneration = 0
			_ = s.Send(s.Parent(), MessageProjectionCommitResult{
				Generation: generation, ProjectionPID: pid, Err: ErrProjectionNotPrepared,
			})
		}
		return nil
	}
	if name != readerActorName(s.opts.Name) || s.readerActor.pid != pid {
		return nil
	}
	s.readerActor.pid = gen.PID{}
	s.readerActor.status.Lifecycle = SnapshotReaderRestarting
	s.readerActor.status.Availability = runtime.AvailabilityUnavailable
	s.readerActor.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.readerActor.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.readerActor.status.Reader.CaughtUp = false
	return nil
}

// HandleMessage routes child status and external projection commit messages.
func (s *SnapshotSupervisor[T]) HandleMessage(from gen.PID, message any) error {
	defer s.reportStatus()

	switch message := message.(type) {
	case MessageSnapshotReaderStatusChanged:
		if from == s.readerActor.pid {
			s.readerActor.status = message.status
		}
	case MessageProjectionStatusChanged:
		if from == s.projectionActor.pid {
			s.projectionActor.status = message.Status
			if s.opts.ProjectionMode == ProjectionCommitExternal {
				message.ProjectionPID = s.projectionActor.pid
				_ = s.Send(s.Parent(), message)
			}
		}
	case MessageProjectionCommit:
		if s.opts.ProjectionMode != ProjectionCommitExternal || from != s.Parent() {
			return nil
		}
		if s.projectionActor.pid == (gen.PID{}) || message.ProjectionPID != s.projectionActor.pid {
			_ = s.Send(s.Parent(), MessageProjectionCommitResult{
				Generation:    message.Generation,
				ProjectionPID: s.projectionActor.pid,
				Err:           ErrProjectionNotPrepared,
			})
			return nil
		}
		s.projectionActor.commitGeneration = message.Generation
		if err := s.Send(s.projectionActor.pid, message); err != nil {
			s.projectionActor.commitGeneration = 0
			_ = s.Send(s.Parent(), MessageProjectionCommitResult{Generation: message.Generation, ProjectionPID: message.ProjectionPID, Err: err})
		}
	case MessageProjectionCommitResult:
		if s.projectionActor.commitGeneration != 0 && from == s.projectionActor.pid && message.Generation == s.projectionActor.commitGeneration && message.ProjectionPID == s.projectionActor.pid {
			s.projectionActor.commitGeneration = 0
			_ = s.Send(s.Parent(), message)
		}
	}
	return nil
}

// HandleCall rejects unsupported synchronous requests.
func (s *SnapshotSupervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot supervisor: unsupported call %T", request), nil
}

// Terminate marks children stopped and reports the shutdown reason.
func (s *SnapshotSupervisor[T]) Terminate(reason error) {
	defer s.reportStatus()
	s.projectionActor.status.Lifecycle = ProjectionActorStopped
	s.projectionActor.status.Availability = runtime.AvailabilityUnavailable
	s.readerActor.status.Lifecycle = SnapshotReaderStopped
	s.readerActor.status.Availability = runtime.AvailabilityUnavailable
	s.readerActor.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.readerActor.status.Reader.Availability = runtime.AvailabilityUnavailable
	if s.opts.Stopped != nil {
		select {
		case s.opts.Stopped <- reason:
		default:
		}
	}
}

// reportStatus publishes the reader actor's current status.
func (s *SnapshotSupervisor[T]) reportStatus() {
	if s.events.statusToken != (gen.Ref{}) {
		_ = s.SendEvent(s.events.Status.Name, s.events.statusToken, s.readerActor.status)
	}
}

// readerActorName returns the stable reader child name.
func readerActorName(name gen.Atom) gen.Atom {
	return gen.Atom(string(name) + "-reader")
}

// projectionActorName returns the stable projection child name.
func projectionActorName(name gen.Atom) gen.Atom {
	return gen.Atom(string(name) + "-projection")
}

// newSnapshotReaderStatus returns the initial unavailable reader status.
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

// newProjectionActorStatus returns the initial unavailable projection status.
func newProjectionActorStatus() ProjectionActorStatus {
	return ProjectionActorStatus{
		Lifecycle:    ProjectionActorStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
}

// defaultOptions fills omitted reader supervisor defaults.
func defaultOptions(opts SnapshotReaderActorOptions) SnapshotReaderActorOptions {
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

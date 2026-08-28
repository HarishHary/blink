package snapshot

import (
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

const (
	readerActorRestartIntensity uint16 = 5
	readerActorRestartPeriod    uint16 = 10
)

// executorReportInterval is how often this executor reports convergence to its controller: a
// quarter of the controller's staleness threshold, so three consecutive lost reports still don't
// read as a dead executor.
const executorReportInterval = 30 * time.Second

// Events identifies the buffered snapshot and status events produced
// by a snapshot supervisor. Each retains only its latest value. Snapshot carries
// *snapshot.Snapshot; Status carries ReaderStatus.
type Events struct {
	Snapshot      gen.Event
	Status        gen.Event
	snapshotToken gen.Ref
	statusToken   gen.Ref
}

// EventsFor returns the stable event identifiers for a snapshot subtree.
func EventsFor(node gen.Node, name gen.Atom) Events {
	return Events{
		Snapshot: gen.Event{Name: gen.Atom(string(name) + "-snapshot"), Node: node.Name()},
		Status:   gen.Event{Name: gen.Atom(string(name) + "-status"), Node: node.Name()},
	}
}

type readerActorState struct {
	pid    gen.PID
	status ReaderActorStatus
}

type projectionActorState struct {
	pid              gen.PID
	commitGeneration int64
	status           ProjectionActorStatus
}

// runtimeSnapshotState tracks the snapshot supervisor incarnation.
type SupervisorState struct {
	Pid   gen.PID
	Epoch uint64
}

// Supervisor owns a reader followed by a typed projection actor.
// Rest-for-one restarts the projection whenever its reader restarts.
type Supervisor[T any] struct {
	act.Supervisor
	opts            SupervisorOptions[T]
	readerActor     readerActorState
	projectionActor projectionActorState
	events          Events
	reportCancel    gen.CancelFunc
}

// ExecutorHeartbeat is this executor's periodic liveness/generation report to the controller.
type ExecutorHeartbeat struct {
	CommittedGeneration int64
	ReadyGeneration     int64
	Availability        string
}

// ExecutorAppliedGeneration reports that a generation went live on this executor: the projection
// committed it, which in external mode means the owning plugin runtime admitted it first.
type ExecutorAppliedGeneration struct {
	Generation int64
	Admitted   bool
}

// MessageExecutorReport carries one executor's convergence report (Heartbeat and/or Applied, either
// may be nil), sent fire-and-forget to the controller actor by the snapshot supervisor. LastError is
// text, not an error: it is rendered into /status and never compared, and EDF reduces an
// unregistered error to its message anyway.
type MessageExecutorReport struct {
	ExecutorID string
	Heartbeat  *ExecutorHeartbeat
	Applied    *ExecutorAppliedGeneration
	LastError  string
}

// MessageExecutorReportTick drives the periodic convergence report to the controller.
type MessageExecutorReportTick struct{}

// NewSupervisor creates a reader/projection supervisor with stable child names.
func NewSupervisor[T any](opts SupervisorOptions[T]) *Supervisor[T] {
	return &Supervisor[T]{opts: optionsWithDefaults(opts)}
}

// Init validates options and configures the supervised reader and projection actors.
func (s *Supervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	defer s.publishStatus()

	if s.opts.ReaderActorOptions.Name == "" || s.opts.ReaderActorOptions.Endpoint.Name == "" || s.opts.ReaderActorOptions.ExecutorID == "" {
		return act.SupervisorSpec{}, fmt.Errorf("actor snapshot: name, endpoint, and executor ID are required")
	}
	if s.opts.Loader == nil {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: loader is required")
	}
	if s.opts.ProjectionMode != ProjectionCommitDirect && s.opts.ProjectionMode != ProjectionCommitExternal {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: invalid commit mode")
	}
	if s.Name() == "" {
		if err := s.RegisterName(s.opts.ReaderActorOptions.Name); err != nil {
			return act.SupervisorSpec{}, fmt.Errorf("register snapshot supervisor %q: %w", s.opts.ReaderActorOptions.Name, err)
		}
	} else if s.Name() != s.opts.ReaderActorOptions.Name {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot supervisor registered as %q, want %q", s.Name(), s.opts.ReaderActorOptions.Name)
	}

	s.events = EventsFor(s.Node(), s.opts.ReaderActorOptions.Name)
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
	s.readerActor.status = newReaderActorStatus()
	s.projectionActor.status = newProjectionActorStatus()
	// Delayed, not immediate: the first report is worth sending only once the reader has had its
	// chance to subscribe, and every state change reports on its own before then.
	if err := s.scheduleExecutorReport(); err != nil {
		return act.SupervisorSpec{}, err
	}

	return act.SupervisorSpec{
		Type:                act.SupervisorTypeRestForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyTransient,
			Intensity: readerActorRestartIntensity,
			Period:    readerActorRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{
			{
				Name: readerActorName(s.opts.ReaderActorOptions.Name),
				Factory: func() gen.ProcessBehavior {
					return newReaderActor(s.opts.ReaderActorOptions)
				},
			},
			{
				Name: projectionActorName(s.opts.ReaderActorOptions.Name),
				Factory: func() gen.ProcessBehavior {
					return &projectionActor[T]{events: s.events, loader: s.opts.Loader, mode: s.opts.ProjectionMode}
				},
			},
		},
	}, nil
}

// HandleChildStart tracks and activates a started reader or projection child.
func (s *Supervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	switch name {
	case projectionActorName(s.opts.ReaderActorOptions.Name):
		if s.projectionActor.pid != (gen.PID{}) {
			return nil
		}
		s.projectionActor.pid = pid
		s.projectionActor.commitGeneration = 0
		s.projectionActor.status = newProjectionActorStatus()
		// Stale child-start callbacks may race a replacement and fail to send.
		_ = s.Send(pid, MessageProjectionActorActivate{})
		return nil
	case readerActorName(s.opts.ReaderActorOptions.Name):
		if s.readerActor.pid != (gen.PID{}) {
			return nil
		}
		s.readerActor.pid = pid
		status := newReaderActorStatus()
		if s.readerActor.status != status {
			s.readerActor.status = status
			s.publishStatus()
		}
		return s.Send(pid, MessageReaderActorActivate{
			snapshotEventName:  s.events.Snapshot.Name,
			snapshotEventToken: s.events.snapshotToken,
		})
	default:
		return nil
	}
}

// HandleChildTerminate records a terminated child and reports external commit failures.
func (s *Supervisor[T]) HandleChildTerminate(_ gen.Atom, pid gen.PID, reason error) error {
	switch pid {
	case s.projectionActor.pid:
		s.projectionActor.pid = gen.PID{}
		s.projectionActor.status.Lifecycle = ProjectionActorRestarting
		s.projectionActor.status.Availability = runtime.AvailabilityUnavailable
		s.projectionActor.status.PreparedGeneration = 0
		if s.opts.ProjectionMode == ProjectionCommitExternal {
			_ = s.Send(s.Parent(), MessageProjectionActorStatusChanged{Status: s.projectionActor.status, ProjectionPID: pid})
		}
		if generation := s.projectionActor.commitGeneration; generation != 0 {
			s.projectionActor.commitGeneration = 0
			_ = s.Send(s.Parent(), MessageProjectionCommitResult{
				Generation: generation, ProjectionPID: pid, Err: ErrProjectionNotPrepared,
			})
		}
		return nil
	case s.readerActor.pid:
		s.readerActor.pid = gen.PID{}
		status := s.readerActor.status
		status.Lifecycle = ReaderActorRestarting
		status.Availability = runtime.AvailabilityUnavailable
		if reason != nil {
			status.LastError = reason.Error()
		}
		s.readerActor.status = status
		s.publishStatus()
		s.reportExecutor(nil)
		return nil
	default:
		return nil
	}
}

// HandleMessage routes child status and external projection commit messages.
func (s *Supervisor[T]) HandleMessage(from gen.PID, message any) error {
	switch message := message.(type) {
	case MessageExecutorReportTick:
		if from != s.PID() {
			return nil
		}
		s.reportExecutor(nil)
		return s.scheduleExecutorReport()
	case MessageReaderActorStatusChanged:
		if from == s.readerActor.pid {
			if s.readerActor.status != message.status {
				attached := s.readerActor.status.Availability != message.status.Availability
				s.readerActor.status = message.status
				s.publishStatus()
				if attached {
					s.reportExecutor(nil)
				}
			}
		}
	case MessageProjectionActorStatusChanged:
		if from == s.projectionActor.pid {
			applied := message.Status.CommittedGeneration > s.projectionActor.status.CommittedGeneration
			s.projectionActor.status = message.Status
			if applied {
				s.reportExecutor(&ExecutorAppliedGeneration{
					Generation: message.Status.CommittedGeneration,
					Admitted:   message.Status.Availability == runtime.AvailabilityReady,
				})
			}
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
func (s *Supervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot supervisor: unsupported call %T", request), nil
}

// HandleInspect exposes concise supervisor operational state: the reader and projection
// children's identity and last-reported status, and any external commit still in flight.
func (s *Supervisor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"supervisor:reader":                          fmt.Sprintf("%s", s.readerActor.pid),
		"supervisor:reader_lifecycle":                string(s.readerActor.status.Lifecycle),
		"supervisor:reader_availability":             string(s.readerActor.status.Availability),
		"supervisor:reader_generation":               fmt.Sprintf("%d", s.readerActor.status.Generation),
		"supervisor:reader_last_error":               s.readerActor.status.LastError,
		"supervisor:reported_availability":           string(s.executorAvailability()),
		"supervisor:projection":                      fmt.Sprintf("%s", s.projectionActor.pid),
		"supervisor:projection_lifecycle":            string(s.projectionActor.status.Lifecycle),
		"supervisor:projection_availability":         string(s.projectionActor.status.Availability),
		"supervisor:projection_committed_generation": fmt.Sprintf("%d", s.projectionActor.status.CommittedGeneration),
		"supervisor:commit_pending":                  fmt.Sprintf("%d", s.projectionActor.commitGeneration),
	}
}

// Terminate marks children stopped and reports the shutdown reason.
func (s *Supervisor[T]) Terminate(reason error) {
	defer s.publishStatus()
	s.cancelExecutorReport()
	s.projectionActor.status.Lifecycle = ProjectionActorStopped
	s.projectionActor.status.Availability = runtime.AvailabilityUnavailable
	s.readerActor.status.Lifecycle = ReaderActorStopped
	s.readerActor.status.Availability = runtime.AvailabilityUnavailable
	if s.opts.Stopped != nil {
		select {
		case s.opts.Stopped <- reason:
		default:
		}
	}
}

// reportExecutor sends this executor's convergence report to its controller, fire-and-forget: the
// controller keeps it for observability only, and the next report supersedes a lost one. The
// supervisor is the one process that sees both halves - the generation the controller pushed to the
// reader, and the generation the projection actually holds live, which in external commit mode is
// the older of the two until the owning runtime admits the new one.
func (s *Supervisor[T]) reportExecutor(applied *ExecutorAppliedGeneration) {
	_ = s.SendProcessID(s.opts.ReaderActorOptions.Endpoint, MessageExecutorReport{
		ExecutorID: s.opts.ReaderActorOptions.ExecutorID,
		Heartbeat: &ExecutorHeartbeat{
			CommittedGeneration: s.readerActor.status.Generation,
			ReadyGeneration:     s.projectionActor.status.CommittedGeneration,
			Availability:        string(s.executorAvailability()),
		},
		Applied:   applied,
		LastError: s.readerActor.status.LastError,
	})
}

// scheduleExecutorReport arms the next periodic report, replacing any already scheduled.
func (s *Supervisor[T]) scheduleExecutorReport() error {
	s.cancelExecutorReport()
	cancel, err := s.SendAfter(s.PID(), MessageExecutorReportTick{}, executorReportInterval)
	if err != nil {
		return fmt.Errorf("schedule executor report: %w", err)
	}
	s.reportCancel = cancel
	return nil
}

// cancelExecutorReport drops any scheduled report.
func (s *Supervisor[T]) cancelExecutorReport() {
	if s.reportCancel != nil {
		s.reportCancel()
		s.reportCancel = nil
	}
}

// executorAvailability is what the controller needs to know about this executor: the projection's
// own health, capped at degraded while the reader cannot receive the next generation.
func (s *Supervisor[T]) executorAvailability() runtime.Availability {
	availability := s.projectionActor.status.Availability
	if availability == runtime.AvailabilityReady && s.readerActor.status.Availability != runtime.AvailabilityReady {
		return runtime.AvailabilityDegraded
	}
	return availability
}

// publishStatus publishes the reader actor's current status.
func (s *Supervisor[T]) publishStatus() {
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

// newReaderActorStatus returns the initial unavailable reader status.
func newReaderActorStatus() ReaderActorStatus {
	return ReaderActorStatus{
		Lifecycle:    ReaderActorStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
}

// newProjectionActorStatus returns the initial unavailable projection status.
func newProjectionActorStatus() ProjectionActorStatus {
	return ProjectionActorStatus{
		Lifecycle:    ProjectionActorStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
}

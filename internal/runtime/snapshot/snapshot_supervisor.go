package snapshot

import (
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

const (
	readerActorRestartIntensity uint16 = 5
	readerActorRestartPeriod    uint16 = 10
)

// executorReportInterval is how often this executor reports convergence to its controller: a
// quarter of the controller's staleness threshold, so three consecutive lost reports still don't
// read as a dead executor.
const executorReportInterval = 30 * time.Second

// SupervisorLifecycle describes the snapshot subtree as a whole. There is no draining stage: no child
// holds external I/O a shutdown must wait out, so a stop is immediate.
type SupervisorLifecycle string

const (
	SupervisorStarting SupervisorLifecycle = "starting"
	SupervisorRunning  SupervisorLifecycle = "running"
	SupervisorStopping SupervisorLifecycle = "stopping"
)

// Events identifies the buffered snapshot and status events produced
// by a snapshot supervisor. Each retains only its latest value. Snapshot carries
// *snapshot.Snapshot; Status carries ReaderStatus.
type Events struct {
	Snapshot      gen.Event
	Status        gen.Event
	snapshotToken gen.Ref
	statusToken   gen.Ref
}

// eventPublication is one registered event and the token SendEvent requires to publish through it:
// the pair the supervisor keeps for what it publishes itself, and hands to the child that publishes
// in its place.
type eventPublication struct {
	name  gen.Atom
	token gen.Ref
}

// registered reports whether the event was registered and its token handed over.
func (p eventPublication) registered() bool { return p.token != (gen.Ref{}) }

// snapshotPublication is what the reader actor publishes committed snapshots through.
func (e Events) snapshotPublication() eventPublication {
	return eventPublication{name: e.Snapshot.Name, token: e.snapshotToken}
}

// statusPublication is what the supervisor publishes reader status through, keeping it itself.
func (e Events) statusPublication() eventPublication {
	return eventPublication{name: e.Status.Name, token: e.statusToken}
}

// EventsFor returns the stable event identifiers for a namespace's snapshot subtree. Both are
// node-local and derived on either side, so neither name is configurable.
func EventsFor(node gen.Node, namespace string) Events {
	return Events{
		Snapshot: gen.Event{Name: subtreeName(namespace, "artifacts"), Node: node.Name()},
		Status:   gen.Event{Name: subtreeName(namespace, "reader-actor-status"), Node: node.Name()},
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
	opts                 SupervisorOptions
	loader               Loader[T]
	lifecycle            SupervisorLifecycle
	readerActor          readerActorState
	projectionActor      projectionActorState
	events               Events
	reportCancel         gen.CancelFunc
	labels               telemetry.Labels
	collectorsRegistered bool
	radarLogged          bool
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

// MessageRadarTick drives the supervisor's periodic radar reconcile.
type MessageRadarTick struct{}

// NewSupervisor creates a reader/projection supervisor named after the namespace it follows.
func NewSupervisor[T any](opts SupervisorOptions, loader Loader[T]) *Supervisor[T] {
	normalized := supervisorOptionsWithDefaults(opts)
	return &Supervisor[T]{opts: normalized, loader: loader, labels: telemetry.NewLabels(normalized.Namespace)}
}

// Init validates options and configures the supervised reader and projection actors.
func (s *Supervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	defer s.publishStatus()
	defer s.publishState()

	// Namespace is required: every process name in this subtree, and every metric label, comes from it.
	if s.opts.Namespace == "" || s.opts.ReaderActorOptions.Endpoint.Name == "" || s.opts.ReaderActorOptions.ExecutorID == "" {
		return act.SupervisorSpec{}, fmt.Errorf("actor snapshot: namespace, endpoint, and executor ID are required")
	}
	if s.loader == nil {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: loader is required")
	}
	if s.opts.ProjectionMode != ProjectionCommitDirect && s.opts.ProjectionMode != ProjectionCommitExternal {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot projection: invalid commit mode")
	}
	name := SupervisorName(s.opts.Namespace)
	if s.Name() == "" {
		if err := s.RegisterName(name); err != nil {
			return act.SupervisorSpec{}, fmt.Errorf("register snapshot supervisor %q: %w", name, err)
		}
	} else if s.Name() != name {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot supervisor registered as %q, want %q", s.Name(), name)
	}

	s.events = EventsFor(s.Node(), s.opts.Namespace)
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
	s.lifecycle = SupervisorStarting
	s.readerActor.status = newReaderActorStatus()
	s.projectionActor.status = newProjectionActorStatus()
	// Delayed, not immediate: the first report is worth sending only once the reader has had its
	// chance to subscribe, and every state change reports on its own before then.
	if err := s.scheduleExecutorReport(); err != nil {
		return act.SupervisorSpec{}, err
	}
	// A message, not an inline call: collectors must exist before a child emits, but radar must not
	// delay the spec.
	if err := s.Send(s.PID(), MessageRadarTick{}); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("snapshot supervisor: schedule radar tick: %w", err)
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
				Name: ReaderActorName(s.opts.Namespace),
				Factory: func() gen.ProcessBehavior {
					return newReaderActor(s.opts.ReaderActorOptions, s.labels, s.events.snapshotPublication())
				},
			},
			{
				Name: ProjectionActorName(s.opts.Namespace),
				Factory: func() gen.ProcessBehavior {
					return newProjectionActor(s.events.Snapshot, s.events.Status, s.loader, s.opts.ProjectionMode, s.labels)
				},
			},
		},
	}, nil
}

// HandleChildStart tracks and activates a started reader or projection child.
func (s *Supervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	defer s.publishState()
	switch name {
	case ProjectionActorName(s.opts.Namespace):
		if s.projectionActor.pid != (gen.PID{}) {
			return nil
		}
		s.labels.Count(s, metricChildStarts, "projection")
		s.projectionActor.pid = pid
		s.projectionActor.commitGeneration = 0
		s.projectionActor.status = newProjectionActorStatus()
		// Stale child-start callbacks may race a replacement and fail to send.
		_ = s.Send(pid, MessageProjectionActorActivate{})
		return nil
	case ReaderActorName(s.opts.Namespace):
		if s.readerActor.pid != (gen.PID{}) {
			return nil
		}
		s.labels.Count(s, metricChildStarts, "reader")
		s.readerActor.pid = pid
		status := newReaderActorStatus()
		if s.readerActor.status != status {
			s.readerActor.status = status
			s.publishStatus()
		}
		return s.Send(pid, MessageReaderActorActivate{})
	default:
		return nil
	}
}

// HandleChildTerminate records a terminated child and reports external commit failures.
func (s *Supervisor[T]) HandleChildTerminate(_ gen.Atom, pid gen.PID, reason error) error {
	defer s.publishState()
	switch pid {
	case s.projectionActor.pid:
		s.labels.Count(s, metricChildTerminations, "projection", telemetry.TerminationReason(reason))
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
		s.labels.Count(s, metricChildTerminations, "reader", telemetry.TerminationReason(reason))
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
	defer s.publishState()
	switch message := message.(type) {
	case MessageExecutorReportTick:
		if from != s.PID() {
			return nil
		}
		s.reportExecutor(nil)
		return s.scheduleExecutorReport()
	case MessageRadarTick:
		if from != s.PID() {
			return nil
		}
		s.reconcileRadar()
		if _, err := s.SendAfter(s.PID(), MessageRadarTick{}, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("reschedule radar tick: %w", err)
		}
		return nil
	case gen.MessageDownProcessID:
		// Forget what a restarted radar lost so the next tick registers it again.
		if message.ProcessID.Name != telemetry.MetricsProcess {
			return nil
		}
		s.collectorsRegistered = false
		s.Log().Debug("radar process down, re-registering on next tick: namespace=%q", s.opts.Namespace)
		return nil
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
		"supervisor:lifecycle":                       string(s.lifecycle),
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
	defer s.publishState()
	s.lifecycle = SupervisorStopping
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
	s.labels.Count(s, metricExecutorReports)
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

// publishState reports every gauge this subtree owns; the supervisor publishes all of them because
// it is the one process holding both children's status.
func (s *Supervisor[T]) publishState() {
	// Both children up promotes the subtree once; a rest-for-one restart replaces a child rather than
	// tearing the subtree down, so it never demotes.
	if s.lifecycle == SupervisorStarting && s.readerActor.pid != (gen.PID{}) && s.projectionActor.pid != (gen.PID{}) {
		s.lifecycle = SupervisorRunning
	}
	subtreeGauges{
		lifecycle:              s.lifecycle,
		readerAvailability:     s.readerActor.status.Availability,
		readerGeneration:       s.readerActor.status.Generation,
		projectionAvailability: s.projectionActor.status.Availability,
		committedGeneration:    s.projectionActor.status.CommittedGeneration,
		preparedGeneration:     s.projectionActor.status.PreparedGeneration,
		reportedAvailability:   s.executorAvailability(),
		generationLag:          s.generationLag(),
		commitPending:          s.projectionActor.commitGeneration,
	}.publish(s.labels, s)
}

// generationLag is what the controller has delivered but this executor does not serve yet, floored at
// zero: a restarted reader reports generation 0 while the projection still serves the last one.
func (s *Supervisor[T]) generationLag() int64 {
	return max(0, s.readerActor.status.Generation-s.projectionActor.status.CommittedGeneration)
}

// reconcileRadar registers this subtree's collectors, retried on every tick until radar accepts them.
func (s *Supervisor[T]) reconcileRadar() {
	if s.collectorsRegistered {
		return
	}
	// Registered through the node: radar deletes a dead registrant's metrics.
	if err := telemetry.Register(s.Node(), subtreeMetrics); err != nil {
		if !s.radarLogged {
			s.radarLogged = true
			s.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", s.opts.Namespace, err)
		}
		return
	}
	s.collectorsRegistered, s.radarLogged = true, false
	// Registered through the node so no child's exit deletes them, monitored so a radar restart does
	// not leave this subtree publishing into collectors that no longer exist.
	if err := s.MonitorProcessID(gen.ProcessID{Name: telemetry.MetricsProcess, Node: s.Node().Name()}); err != nil {
		s.Log().Debug("radar monitor unavailable: namespace=%q error=%v", s.opts.Namespace, err)
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
	if status := s.events.statusPublication(); status.registered() {
		_ = s.SendEvent(status.name, status.token, s.readerActor.status)
	}
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

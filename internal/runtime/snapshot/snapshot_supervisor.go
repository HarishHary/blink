package snapshot

import (
	"errors"
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

const (
	readerActorRestartIntensity uint16 = 5
	readerActorRestartPeriod    uint16 = 10
)

// executorReportInterval is a quarter of the controller's staleness threshold, so three lost
// reports still do not read as a dead executor.
const executorReportInterval = 30 * time.Second

// SupervisorLifecycle describes the subtree as a whole; no child holds I/O a stop must wait out, so
// there is no draining stage.
type SupervisorLifecycle string

const (
	SupervisorStarting SupervisorLifecycle = "starting"
	SupervisorRunning  SupervisorLifecycle = "running"
	SupervisorStopped  SupervisorLifecycle = "stopped"
)

// eventPublication is one registered event and the token SendEvent requires to publish through it.
type eventPublication struct {
	name  gen.Atom
	token gen.Ref
}

type readerActorState struct {
	pid         gen.PID
	status      ReaderActorStatus
	statusEpoch int64
}

type projectionActorState struct {
	pid              gen.PID
	commitGeneration int64
	status           ProjectionActorStatus
	statusEpoch      int64
}

// SupervisorStatus reports the health of the entire snapshot subtree.
type SupervisorStatus struct {
	Lifecycle    SupervisorLifecycle
	Availability runtime.Availability
	LastError    error
}

// SupervisorState identifies one snapshot supervisor incarnation.
type SupervisorState struct {
	Pid   gen.PID
	Epoch uint64
}

// Supervisor owns a reader followed by a typed projection actor, rest-for-one so a reader restart
// restarts the projection with it.
type Supervisor[T any] struct {
	act.Supervisor
	opts                 SupervisorOptions
	loader               Loader[T]
	readerActor          readerActorState
	projectionActor      projectionActorState
	snapshotEvent        eventPublication
	statusEvent          eventPublication
	reportCancel         gen.CancelFunc
	collectorsRegistered bool
	radarLogged          bool
	labels               telemetry.Labels
	signal               telemetry.Signal
	lastStatus           SupervisorStatus
	lastError            error
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// ExecutorHeartbeat is this executor's periodic liveness/generation report to the controller.
type ExecutorHeartbeat struct {
	CommittedGeneration int64
	ReadyGeneration     int64
	Availability        string
}

// ExecutorAppliedGeneration reports that the projection committed a generation, which in external
// mode means the owning runtime admitted it first.
type ExecutorAppliedGeneration struct {
	Generation int64
	Admitted   bool
}

// MessageExecutorReport carries one executor's convergence report, either half nil, sent fire-and-forget to the controller actor.
type MessageExecutorReport struct {
	ExecutorID string
	Heartbeat  *ExecutorHeartbeat
	Applied    *ExecutorAppliedGeneration
	LastError  error
}

// MessageExecutorReportTick drives the periodic convergence report to the controller.
type MessageExecutorReportTick struct{}

// MessageRadarTick drives the supervisor's periodic radar reconcile.
type MessageRadarTick struct{}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// NewSupervisor creates a reader/projection supervisor named after the namespace it follows.
func NewSupervisor[T any](opts SupervisorOptions, loader Loader[T]) *Supervisor[T] {
	normalized := supervisorOptionsWithDefaults(opts)
	return &Supervisor[T]{
		opts:   normalized,
		loader: loader,
		labels: telemetry.NewLabels(normalized.Namespace),
		signal: newHealthSignal(normalized.Namespace),
	}
}

// Init validates options and configures the supervised reader and projection actors.
func (s *Supervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	defer s.reconcileStatus()

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

	artifactsEvent := ArtifactsEventFor(s.Node(), s.opts.Namespace)
	statusEvent := ReaderActorStatusEventFor(s.Node(), s.opts.Namespace)
	// Keep only the latest snapshot available to a restarted projection.
	snapshotToken, err := s.RegisterEvent(artifactsEvent.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot event: %w", err)
	}
	s.snapshotEvent = eventPublication{name: artifactsEvent.Name, token: snapshotToken}
	statusToken, err := s.RegisterEvent(statusEvent.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot status event: %w", err)
	}
	s.statusEvent = eventPublication{name: statusEvent.Name, token: statusToken}
	s.lastStatus.Lifecycle = SupervisorStarting
	s.reconcileReaderStatus(newReaderActorStatus())
	s.projectionActor.status = newProjectionActorStatus()
	// Delayed: the first report is worth sending only once the reader has had its chance to subscribe.
	if err := s.scheduleExecutorReport(); err != nil {
		return act.SupervisorSpec{}, err
	}
	// A message, not an inline call: radar must not delay the spec.
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
					return newReaderActor(s.opts.ReaderActorOptions, s.labels, s.snapshotEvent)
				},
			},
			{
				Name: ProjectionActorName(s.opts.Namespace),
				Factory: func() gen.ProcessBehavior {
					return newProjectionActor(artifactsEvent, statusEvent, s.loader, s.opts.ProjectionMode, s.labels)
				},
			},
		},
	}, nil
}

// HandleChildStart tracks and activates a started reader or projection child.
func (s *Supervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	defer s.reconcileStatus()
	switch name {
	case ProjectionActorName(s.opts.Namespace):
		if s.projectionActor.pid != (gen.PID{}) {
			return nil
		}
		s.labels.Count(s, metricChildStarts, "projection")
		s.projectionActor.pid = pid
		s.projectionActor.commitGeneration = 0
		s.projectionActor.status = newProjectionActorStatus()
		// The replacement continues after this identity change, even if the clock moved backward.
		s.projectionActor.statusEpoch = runtime.NextStatusEpoch(s.projectionActor.statusEpoch)
		// Stale child-start callbacks may race a replacement and fail to send.
		_ = s.SendWithPriority(pid, MessageProjectionActorActivate{StatusEpoch: s.projectionActor.statusEpoch}, gen.MessagePriorityHigh)
		return nil
	case ReaderActorName(s.opts.Namespace):
		if s.readerActor.pid != (gen.PID{}) {
			return nil
		}
		s.labels.Count(s, metricChildStarts, "reader")
		s.readerActor.pid = pid
		s.reconcileReaderStatus(newReaderActorStatus())
		return s.SendWithPriority(pid, MessageReaderActorActivate{StatusEpoch: s.readerActor.statusEpoch}, gen.MessagePriorityHigh)
	default:
		return nil
	}
}

// HandleChildTerminate records a terminated child and reports external commit failures.
func (s *Supervisor[T]) HandleChildTerminate(_ gen.Atom, pid gen.PID, reason error) error {
	defer s.reconcileStatus()
	switch pid {
	case s.projectionActor.pid:
		s.labels.Count(s, metricChildTerminations, "projection", telemetry.TerminationReason(reason))
		s.projectionActor.pid = gen.PID{}
		status := s.projectionActor.status
		status.Lifecycle = ProjectionActorRestarting
		status.Availability = runtime.AvailabilityUnavailable
		status.PreparedGeneration = 0
		status.LastError = reason
		s.reconcileProjectionStatus(status, pid)
		if generation := s.projectionActor.commitGeneration; generation != 0 {
			s.projectionActor.commitGeneration = 0
			_ = s.SendWithPriority(s.Parent(), MessageProjectionCommitResult{
				Generation: generation, ProjectionPID: pid, Err: ErrProjectionNotPrepared,
			}, gen.MessagePriorityHigh)
		}
		return nil
	case s.readerActor.pid:
		s.labels.Count(s, metricChildTerminations, "reader", telemetry.TerminationReason(reason))
		s.readerActor.pid = gen.PID{}
		status := s.readerActor.status
		status.Lifecycle = ReaderActorRestarting
		status.Availability = runtime.AvailabilityUnavailable
		if reason != nil {
			status.LastError = reason
		}
		s.reconcileReaderStatus(status)
		s.propagateExecutorStatus(nil)
		return nil
	default:
		return nil
	}
}

// HandleMessage routes child status and external projection commit messages.
func (s *Supervisor[T]) HandleMessage(from gen.PID, message any) error {
	defer s.reconcileStatus()
	switch message := message.(type) {
	case MessageExecutorReportTick:
		if from != s.PID() {
			return nil
		}
		s.propagateExecutorStatus(nil)
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
		switch message.ProcessID.Name {
		case telemetry.MetricsProcess:
			s.collectorsRegistered = false
		case telemetry.HealthProcess:
			s.signal = newHealthSignal(s.opts.Namespace)
		default:
			return nil
		}
		s.Log().Debug("radar process down, re-registering on next tick: namespace=%q process=%s", s.opts.Namespace, message.ProcessID.Name)
		return nil
	case MessageReaderActorStatusChanged:
		if from == s.readerActor.pid && message.StatusEpoch > s.readerActor.statusEpoch {
			changed := !sameReaderActorStatus(s.readerActor.status, message.Status)
			attached := s.readerActor.status.Availability != message.Status.Availability || runtime.ErrorText(s.readerActor.status.LastError) != runtime.ErrorText(message.Status.LastError)
			s.readerActor.statusEpoch = message.StatusEpoch
			s.readerActor.status = message.Status
			if changed {
				s.propagateStatus(message.Status)
			}
			if attached {
				s.propagateExecutorStatus(nil)
			}
		}
	case MessageProjectionActorStatusChanged:
		if from == s.projectionActor.pid && message.StatusEpoch > s.projectionActor.statusEpoch {
			errorChanged := runtime.ErrorText(s.projectionActor.status.LastError) != runtime.ErrorText(message.Status.LastError)
			applied := message.Status.CommittedGeneration > s.projectionActor.status.CommittedGeneration
			s.projectionActor.statusEpoch = message.StatusEpoch
			s.projectionActor.status = message.Status
			s.propagateProjectionStatus(message.Status, s.projectionActor.pid)
			if applied {
				s.propagateExecutorStatus(&ExecutorAppliedGeneration{
					Generation: message.Status.CommittedGeneration,
					Admitted:   message.Status.Availability == runtime.AvailabilityReady,
				})
			} else if errorChanged {
				s.propagateExecutorStatus(nil)
			}
		}
	case MessageProjectionCommit:
		if s.opts.ProjectionMode != ProjectionCommitExternal || from != s.Parent() {
			return nil
		}
		if s.projectionActor.pid == (gen.PID{}) || message.ProjectionPID != s.projectionActor.pid {
			_ = s.SendWithPriority(s.Parent(), MessageProjectionCommitResult{
				Generation:    message.Generation,
				ProjectionPID: s.projectionActor.pid,
				Err:           ErrProjectionNotPrepared,
			}, gen.MessagePriorityHigh)
			return nil
		}
		s.projectionActor.commitGeneration = message.Generation
		if err := s.SendWithPriority(s.projectionActor.pid, message, gen.MessagePriorityHigh); err != nil {
			s.projectionActor.commitGeneration = 0
			_ = s.SendWithPriority(s.Parent(), MessageProjectionCommitResult{Generation: message.Generation, ProjectionPID: message.ProjectionPID, Err: err}, gen.MessagePriorityHigh)
		}
	case MessageProjectionCommitResult:
		if s.projectionActor.commitGeneration != 0 && from == s.projectionActor.pid && message.Generation == s.projectionActor.commitGeneration && message.ProjectionPID == s.projectionActor.pid {
			s.projectionActor.commitGeneration = 0
			_ = s.SendWithPriority(s.Parent(), message, gen.MessagePriorityHigh)
		}
	}
	return nil
}

// HandleCall rejects unsupported synchronous requests.
func (s *Supervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot supervisor: unsupported call %T", request), nil
}

// Terminate marks children stopped and reports the shutdown reason.
func (s *Supervisor[T]) Terminate(reason error) {
	defer s.reconcileStatus()
	s.lastStatus.Lifecycle = SupervisorStopped
	s.lastError = reason
	s.cancelExecutorReport()
	s.projectionActor.status.Lifecycle = ProjectionActorStopped
	s.projectionActor.status.Availability = runtime.AvailabilityUnavailable
	status := s.readerActor.status
	status.Lifecycle = ReaderActorStopped
	status.Availability = runtime.AvailabilityUnavailable
	s.reconcileReaderStatus(status)
	if s.opts.Stopped != nil {
		select {
		case s.opts.Stopped <- reason:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// registered reports whether the event was registered and its token handed over.
func (p eventPublication) registered() bool { return p.token != (gen.Ref{}) }

// ArtifactsEventFor names the buffered event a namespace's reader publishes *Snapshot through,
// derived on either side rather than configured.
func ArtifactsEventFor(node gen.Node, namespace string) gen.Event {
	return gen.Event{Name: subtreeName(namespace, "artifacts"), Node: node.Name()}
}

// ReaderActorStatusEventFor names the buffered event the supervisor publishes ReaderActorStatus
// through, derived the same way.
func ReaderActorStatusEventFor(node gen.Node, namespace string) gen.Event {
	return gen.Event{Name: subtreeName(namespace, "reader-actor-status"), Node: node.Name()}
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

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// HandleInspect exposes both children's identity and last-reported status plus any in-flight commit.
func (s *Supervisor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"supervisor:last_error":                      runtime.ErrorText(s.status().LastError),
		"supervisor:projection_last_error":           runtime.ErrorText(s.projectionActor.status.LastError),
		"supervisor:lifecycle":                       string(s.lastStatus.Lifecycle),
		"supervisor:readiness_signal":                s.signal.State(),
		"supervisor:reader":                          fmt.Sprintf("%s", s.readerActor.pid),
		"supervisor:reader_lifecycle":                string(s.readerActor.status.Lifecycle),
		"supervisor:reader_availability":             string(s.readerActor.status.Availability),
		"supervisor:reader_generation":               fmt.Sprintf("%d", s.readerActor.status.Generation),
		"supervisor:reader_last_error":               runtime.ErrorText(s.readerActor.status.LastError),
		"supervisor:reported_availability":           string(s.executorAvailability()),
		"supervisor:projection":                      fmt.Sprintf("%s", s.projectionActor.pid),
		"supervisor:projection_lifecycle":            string(s.projectionActor.status.Lifecycle),
		"supervisor:projection_availability":         string(s.projectionActor.status.Availability),
		"supervisor:projection_committed_generation": fmt.Sprintf("%d", s.projectionActor.status.CommittedGeneration),
		"supervisor:commit_pending":                  fmt.Sprintf("%d", s.projectionActor.commitGeneration),
	}
}

// reconcileStatus advances lifecycle and refreshes gauges/readiness from the reconciled child states.
func (s *Supervisor[T]) reconcileStatus() {
	// Both children up promotes the subtree once, and a rest-for-one restart never demotes it.
	if s.lastStatus.Lifecycle == SupervisorStarting && s.readerActor.pid != (gen.PID{}) && s.projectionActor.pid != (gen.PID{}) {
		s.lastStatus.Lifecycle = SupervisorRunning
	}
	s.lastStatus = s.status()
	s.publishGauges()
	s.propagateReadiness()
}

// reconcileReaderStatus compares before replacing child status, then versions and publishes changes.
func (s *Supervisor[T]) reconcileReaderStatus(next ReaderActorStatus) {
	if sameReaderActorStatus(s.readerActor.status, next) {
		return
	}
	s.readerActor.statusEpoch = runtime.NextStatusEpoch(s.readerActor.statusEpoch)
	s.readerActor.status = next
	s.propagateStatus(next)
}

// publishGauges publishes current values without changing lifecycle or propagating status.
func (s *Supervisor[T]) publishGauges() {
	subtreeGauges{
		lifecycle:              s.lastStatus.Lifecycle,
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

// generationLag is what the controller delivered but this executor does not serve yet, floored at
// zero for a restarted reader.
func (s *Supervisor[T]) generationLag() int64 {
	return max(0, s.readerActor.status.Generation-s.projectionActor.status.CommittedGeneration)
}

// reconcileRadar registers whatever radar is still missing, then heartbeats the readiness signal.
func (s *Supervisor[T]) reconcileRadar() {
	if !s.collectorsRegistered {
		// Registered through the node: radar deletes a dead registrant's metrics.
		if err := telemetry.Register(s.Node(), subtreeMetrics); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
		s.collectorsRegistered = true
		s.watchRadar(telemetry.MetricsProcess)
	}
	if !s.signal.Registered() {
		if !s.watchRadar(telemetry.HealthProcess) {
			return
		}
		if err := s.signal.Register(s); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
	}
	s.radarLogged = false
	s.propagateReadiness()
	s.signal.Heartbeat(s)
}

// watchRadar monitors one radar process and reports whether the watch is installed.
func (s *Supervisor[T]) watchRadar(name gen.Atom) bool {
	if err := s.MonitorProcessID(gen.ProcessID{Name: name, Node: s.Node().Name()}); err != nil && !errors.Is(err, gen.ErrTargetExist) {
		s.Log().Debug("radar monitor unavailable: namespace=%q process=%s error=%v", s.opts.Namespace, name, err)
		return false
	}
	return true
}

// radarUnavailableOnce logs only the first failure of an outage.
func (s *Supervisor[T]) radarUnavailableOnce(err error) {
	if s.radarLogged {
		return
	}
	s.radarLogged = true
	s.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", s.opts.Namespace, err)
}

// executorAvailability is the projection's health, capped at degraded while the reader is detached.
func (s *Supervisor[T]) executorAvailability() runtime.Availability {
	availability := s.projectionActor.status.Availability
	if availability == runtime.AvailabilityReady && s.readerActor.status.Availability != runtime.AvailabilityReady {
		return runtime.AvailabilityDegraded
	}
	return availability
}

// propagateStatus forwards the supplied reader status through the registered event.
func (s *Supervisor[T]) propagateStatus(next ReaderActorStatus) {
	if s.statusEvent.registered() {
		_ = s.SendEvent(s.statusEvent.name, s.statusEvent.token, MessageReaderActorStatusChanged{StatusEpoch: s.readerActor.statusEpoch, Status: next})
	}
}

// propagateReadiness includes both children, independently of reader-event deduplication.
func (s *Supervisor[T]) propagateReadiness() {
	s.signal.SetReady(s, s.lastStatus.Lifecycle == SupervisorRunning &&
		s.readerActor.pid != (gen.PID{}) && s.projectionActor.pid != (gen.PID{}) &&
		s.executorAvailability() == runtime.AvailabilityReady)
}

// reconcileProjectionStatus versions projection health; child start versions identity changes.
func (s *Supervisor[T]) reconcileProjectionStatus(next ProjectionActorStatus, pid gen.PID) {
	if s.projectionActor.statusEpoch == 0 || !sameProjectionActorStatus(s.projectionActor.status, next) {
		s.projectionActor.statusEpoch = runtime.NextStatusEpoch(s.projectionActor.statusEpoch)
		s.projectionActor.status = next
	}
	s.propagateProjectionStatus(next, pid)
}

// propagateProjectionStatus forwards the reconciled projection version without changing its epoch.
func (s *Supervisor[T]) propagateProjectionStatus(next ProjectionActorStatus, pid gen.PID) {
	if s.opts.ProjectionMode == ProjectionCommitExternal {
		_ = s.SendWithPriority(s.Parent(), MessageProjectionActorStatusChanged{StatusEpoch: s.projectionActor.statusEpoch, Status: next, ProjectionPID: pid}, gen.MessagePriorityHigh)
	}
}

// status derives subtree health without changing either child's status.
func (s *Supervisor[T]) status() SupervisorStatus {
	return SupervisorStatus{
		Lifecycle:    s.lastStatus.Lifecycle,
		Availability: s.executorAvailability(),
		LastError:    runtime.FirstError(s.lastError, s.projectionActor.status.LastError, s.readerActor.status.LastError),
	}
}

// propagateExecutorStatus sends this executor's current convergence report.
// Both periodic and on-change reports are normal-priority bookkeeping, not commit acknowledgements.
func (s *Supervisor[T]) propagateExecutorStatus(applied *ExecutorAppliedGeneration) {
	s.labels.Count(s, metricExecutorReports)
	//argus:allow A1001 fresh heartbeat and applied generation transfer exclusively to the controller
	_ = s.Send(s.opts.ReaderActorOptions.Endpoint, MessageExecutorReport{
		ExecutorID: s.opts.ReaderActorOptions.ExecutorID,
		Heartbeat: &ExecutorHeartbeat{
			CommittedGeneration: s.readerActor.status.Generation,
			ReadyGeneration:     s.projectionActor.status.CommittedGeneration,
			Availability:        string(s.executorAvailability()),
		},
		Applied:   applied,
		LastError: s.status().LastError,
	})
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

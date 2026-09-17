package stage

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// processorChildCount is the fixed direct-child count: reader, writer, DLQ writer, coordinator, job pool.
const processorChildCount = 5

// Child labels for the processor supervisor's own metrics, stable across incarnations.
const (
	childRoleReader      = "reader"
	childRoleCoordinator = "coordinator"
	childRoleJobPool     = "job_pool"
	childRoleWriter      = "writer:"
)

// CoordinatorLifecycle describes a coordinator lifecycle state.
type CoordinatorLifecycle string

const (
	CoordinatorStarting CoordinatorLifecycle = "starting"
	CoordinatorRunning  CoordinatorLifecycle = "running"
	CoordinatorDraining CoordinatorLifecycle = "draining"
	CoordinatorDrained  CoordinatorLifecycle = "drained"
	CoordinatorStopped  CoordinatorLifecycle = "stopped"
)

// CoordinatorStatus reports coordinator lifecycle, availability, and failure. It is exported because
// the coordinator is caller-supplied and constructs it.
type CoordinatorStatus struct {
	Lifecycle    CoordinatorLifecycle
	Availability runtime.Availability
	Err          error
}

// ProcessorSupervisorLifecycle describes a processor-supervisor lifecycle state.
type ProcessorSupervisorLifecycle string

const (
	ProcessorSupervisorStarting ProcessorSupervisorLifecycle = "starting"
	ProcessorSupervisorRunning  ProcessorSupervisorLifecycle = "running"
	ProcessorSupervisorStopped  ProcessorSupervisorLifecycle = "stopped"
)

// ProcessorSupervisorStatus reports processor-supervisor lifecycle and availability. It is exported
// because the subtree reports it to a parent outside this package.
type ProcessorSupervisorStatus struct {
	Lifecycle    ProcessorSupervisorLifecycle
	Availability runtime.Availability
	Err          error
}

// childState tracks one direct child incarnation and the last status it reported.
type childState[S any] struct {
	pid            gen.PID
	statusEpoch    int64
	activationSent bool
	status         S
}

// Direct child states; only the Kafka children use activationSent.
type (
	kafkaReaderState = childState[KafkaReaderStatus]
	kafkaWriterState = childState[kafkaWriterStatus]
	coordinatorState = childState[CoordinatorStatus]
	jobPoolState     = childState[jobPoolStatus]
)

// childSnapshot projects one child for aggregation and inspection, never for storage.
type childSnapshot struct {
	name         gen.Atom
	pid          gen.PID
	lifecycle    string
	availability runtime.Availability
	err          error
}

// processorIOFence tracks I/O completion for a child incarnation.
type processorIOFence struct {
	owner      gen.PID
	name       gen.Atom
	completion *runtime.IOBarrier
}

// processorSupervisor waits for current child statuses before reporting readiness.
type processorSupervisor struct {
	act.Supervisor
	opts                 ProcessorSupervisorOptions
	newReader            func() brokers.Reader
	newWriter            func() brokers.Writer
	newDLQWriter         func() brokers.Writer
	barrier              *runtime.IOBarrier
	reader               kafkaReaderState
	writer               kafkaWriterState
	dlqWriter            kafkaWriterState
	coordinator          coordinatorState
	jobPool              jobPoolState
	readerFences         map[gen.Alias]processorIOFence
	writerFences         map[gen.Alias]processorIOFence
	lifecycle            ProcessorSupervisorLifecycle
	teardownPending      bool
	collectorsRegistered bool
	radarLogged          bool
	labels               telemetry.Labels
	signal               telemetry.Signal
	err                  error                     // the supervisor's own failure, kept apart from its children's errors
	lastStatus           ProcessorSupervisorStatus // last published projection, the baseline reconcileStatus dedupes against
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageRadarTick triggers processor supervisor reconciliation.
// It stays high priority because it also releases completed I/O fences and unblocks replacements.
type MessageRadarTick struct{}

// MessageCoordinatorStatusRequest requests a coordinator's current status.
type MessageCoordinatorStatusRequest struct{}

// MessageCoordinatorStatusChanged reports a coordinator's current status.
type MessageCoordinatorStatusChanged struct {
	StatusEpoch int64
	Status      CoordinatorStatus
}

// MessageProcessorSupervisorStatusRequest requests a processor supervisor's current status.
type MessageProcessorSupervisorStatusRequest struct{}

// MessageProcessorSupervisorStatusChanged reports a processor supervisor's current status.
type MessageProcessorSupervisorStatusChanged struct {
	Status ProcessorSupervisorStatus
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// NewProcessorSupervisor creates a supervisor with one output and one DLQ writer.
func NewProcessorSupervisor(opts ProcessorSupervisorOptions, newReader func() brokers.Reader, newWriter, newDLQWriter func() brokers.Writer, barrier *runtime.IOBarrier) gen.ProcessBehavior {
	opts.JobPool.WorkerArgs = append([]any(nil), opts.JobPool.WorkerArgs...)
	return &processorSupervisor{
		opts:         opts,
		newReader:    newReader,
		newWriter:    newWriter,
		newDLQWriter: newDLQWriter,
		barrier:      barrier,
		labels:       telemetry.NewLabels(opts.Namespace),
		signal:       newHealthSignal(opts.Namespace),
	}
}

// Init initializes the supervisor child specification and radar.
func (s *processorSupervisor) Init(...any) (act.SupervisorSpec, error) {
	if s.barrier == nil {
		return act.SupervisorSpec{}, fmt.Errorf("processor supervisor: I/O barrier is required")
	}
	if s.newReader == nil {
		return act.SupervisorSpec{}, fmt.Errorf("processor supervisor: reader factory is required")
	}
	if s.newWriter == nil {
		return act.SupervisorSpec{}, fmt.Errorf("processor supervisor: output writer factory is required")
	}
	if s.newDLQWriter == nil {
		return act.SupervisorSpec{}, fmt.Errorf("processor supervisor: DLQ writer factory is required")
	}
	s.opts = processorSupervisorOptionsWithDefaults(s.opts)
	if err := validateProcessorSupervisorOptions(s.opts); err != nil {
		return act.SupervisorSpec{}, err
	}
	previous := s.lastStatus
	s.lifecycle = ProcessorSupervisorStarting
	s.reader = kafkaReaderState{status: KafkaReaderStatus{Lifecycle: KafkaReaderStarting, Availability: runtime.AvailabilityUnavailable}}
	s.writer = kafkaWriterState{status: kafkaWriterStatus{lifecycle: KafkaWriterStarting, availability: runtime.AvailabilityUnavailable}}
	s.dlqWriter = kafkaWriterState{status: kafkaWriterStatus{lifecycle: KafkaWriterStarting, availability: runtime.AvailabilityUnavailable}}
	s.coordinator = coordinatorState{status: CoordinatorStatus{Lifecycle: CoordinatorStarting, Availability: runtime.AvailabilityUnavailable}}
	s.jobPool = jobPoolState{status: jobPoolStatus{lifecycle: JobPoolStarting, availability: runtime.AvailabilityUnavailable}}
	s.readerFences = make(map[gen.Alias]processorIOFence)
	s.writerFences = make(map[gen.Alias]processorIOFence)
	s.reconcileStatus(previous)
	if err := s.SendWithPriority(s.PID(), MessageRadarTick{}, gen.MessagePriorityHigh); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("processor supervisor: schedule radar tick: %w", err)
	}
	return act.SupervisorSpec{
		Type:              act.SupervisorTypeAllForOne,
		EnableHandleChild: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyPermanent,
			Intensity: s.opts.RestartIntensity,
			Period:    s.opts.RestartPeriod,
			KeepOrder: true,
		},
		Children: s.childSpecs(),
	}, nil
}

// HandleMessage handles child status, I/O, and radar messages.
func (s *processorSupervisor) HandleMessage(from gen.PID, message any) error {
	switch message := message.(type) {
	case MessageRadarTick:
		if from != s.PID() || s.lifecycle == ProcessorSupervisorStopped {
			return nil
		}
		if err := s.pollIOCompletions(); err != nil {
			return err
		}
		s.reconcileRadar()
		s.requestChildStatuses()
		s.publishGauges()
		if _, err := s.SendWithPriorityAfter(s.PID(), MessageRadarTick{}, gen.MessagePriorityHigh, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("processor supervisor: reschedule radar tick: %w", err)
		}
	case gen.MessageDownProcessID:
		if message.ProcessID.Node != s.Node().Name() {
			return nil
		}
		switch message.ProcessID.Name {
		case telemetry.MetricsProcess:
			s.collectorsRegistered = false
		case telemetry.HealthProcess:
			s.signal = newHealthSignal(s.opts.Namespace)
		}
	case MessageProcessorSupervisorStatusRequest:
		if from == s.Parent() {
			previous := s.lastStatus
			s.reconcileStatus(previous)
			if sameProcessorSupervisorStatus(previous, s.lastStatus) {
				s.propagateStatus(s.lastStatus)
			}
		}
	case MessageCoordinatorStatusChanged:
		s.acceptChildStatus(acceptStatus(&s.coordinator, from, message.StatusEpoch, message.Status))
	case MessageJobPoolStatusChanged:
		s.acceptChildStatus(acceptStatus(&s.jobPool, from, message.statusEpoch, message.status))
	case MessageKafkaReaderStatusChanged:
		s.acceptChildStatus(acceptStatus(&s.reader, from, message.StatusEpoch, message.Status))
	case MessageKafkaWriterStatusChanged:
		s.acceptChildStatus(acceptStatus(&s.writer, from, message.statusEpoch, message.status) ||
			acceptStatus(&s.dlqWriter, from, message.statusEpoch, message.status))
	case MessageKafkaReaderIOStarted:
		if s.isChild(from, s.reader.pid, s.readerName()) {
			s.registerIOFence(from, s.readerName(), message.Alias, message.completion, s.readerFences)
		}
	case MessageKafkaReaderIOStopped:
		return s.completeIOFence(from, message.Alias, message, s.readerFences)
	case MessageKafkaWriterIOStarted:
		if name := s.writerChildName(from); name != "" {
			s.registerIOFence(from, name, message.Alias, message.completion, s.writerFences)
		}
	case MessageKafkaWriterIOStopped:
		return s.completeIOFence(from, message.Alias, message, s.writerFences)
	}
	return nil
}

// acceptStatus records a status the current child incarnation reported after its last one.
func acceptStatus[S any](state *childState[S], from gen.PID, epoch int64, status S) bool {
	if state.pid != from || state.pid == (gen.PID{}) || epoch <= state.statusEpoch {
		return false
	}
	state.statusEpoch = epoch
	state.status = status
	return true
}

// acceptChildStatus reconciles only when a child status was actually recorded.
func (s *processorSupervisor) acceptChildStatus(accepted bool) {
	if accepted {
		s.reconcileStatus(s.lastStatus)
	}
}

// HandleChildStart records a started direct child.
func (s *processorSupervisor) HandleChildStart(name gen.Atom, pid gen.PID) error {
	var role string
	switch name {
	case s.readerName():
		if s.reader.pid != (gen.PID{}) {
			return nil
		}
		s.reader = kafkaReaderState{pid: pid, status: KafkaReaderStatus{
			Lifecycle:    KafkaReaderStarting,
			Availability: runtime.AvailabilityUnavailable,
			Err:          s.reader.status.Err,
		}}
		role = childRoleReader
	case s.writerName():
		if s.writer.pid != (gen.PID{}) {
			return nil
		}
		s.writer = kafkaWriterState{pid: pid, status: kafkaWriterStatus{
			lifecycle:    KafkaWriterStarting,
			availability: runtime.AvailabilityUnavailable,
			err:          s.writer.status.err,
		}}
		role = childRoleWriter + s.opts.Writer.Destination
	case s.dlqWriterName():
		if s.dlqWriter.pid != (gen.PID{}) {
			return nil
		}
		s.dlqWriter = kafkaWriterState{pid: pid, status: kafkaWriterStatus{
			lifecycle:    KafkaWriterStarting,
			availability: runtime.AvailabilityUnavailable,
			err:          s.dlqWriter.status.err,
		}}
		role = childRoleWriter + s.opts.DLQWriter.Destination
	case s.coordinatorName():
		if s.coordinator.pid != (gen.PID{}) {
			return nil
		}
		s.coordinator = coordinatorState{pid: pid, status: CoordinatorStatus{
			Lifecycle:    CoordinatorStarting,
			Availability: runtime.AvailabilityUnavailable,
			Err:          s.coordinator.status.Err,
		}}
		role = childRoleCoordinator
	case s.jobPoolName():
		if s.jobPool.pid != (gen.PID{}) {
			return nil
		}
		s.jobPool = jobPoolState{pid: pid, status: jobPoolStatus{
			lifecycle:    JobPoolStarting,
			availability: runtime.AvailabilityUnavailable,
			err:          s.jobPool.status.err,
		}}
		role = childRoleJobPool
	default:
		return nil
	}
	previous := s.lastStatus
	defer s.reconcileStatus(previous)
	s.requestChildStatus(name, pid)
	if err := s.activateKafkaChildren(); err != nil {
		return err
	}
	s.labels.Count(s, metricProcessorChildStarts, role)
	return nil
}

// HandleChildTerminate handles direct child termination.
func (s *processorSupervisor) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	var role string
	switch name {
	case s.readerName():
		if s.reader.pid != pid {
			return nil
		}
		s.reader = kafkaReaderState{status: KafkaReaderStatus{
			Lifecycle: KafkaReaderStopped, Availability: runtime.AvailabilityUnavailable, Err: reason,
		}}
		role = childRoleReader
	case s.writerName():
		if s.writer.pid != pid {
			return nil
		}
		s.writer = kafkaWriterState{status: kafkaWriterStatus{
			lifecycle: KafkaWriterStopped, availability: runtime.AvailabilityUnavailable, err: reason,
		}}
		role = childRoleWriter + s.opts.Writer.Destination
	case s.dlqWriterName():
		if s.dlqWriter.pid != pid {
			return nil
		}
		s.dlqWriter = kafkaWriterState{status: kafkaWriterStatus{
			lifecycle: KafkaWriterStopped, availability: runtime.AvailabilityUnavailable, err: reason,
		}}
		role = childRoleWriter + s.opts.DLQWriter.Destination
	case s.coordinatorName():
		if s.coordinator.pid != pid {
			return nil
		}
		s.coordinator = coordinatorState{status: CoordinatorStatus{
			Lifecycle: CoordinatorStopped, Availability: runtime.AvailabilityUnavailable, Err: reason,
		}}
		role = childRoleCoordinator
	case s.jobPoolName():
		if s.jobPool.pid != pid {
			return nil
		}
		s.jobPool = jobPoolState{status: jobPoolStatus{
			lifecycle: JobPoolStopped, availability: runtime.AvailabilityUnavailable, err: reason,
		}}
		role = childRoleJobPool
	default:
		return nil
	}
	previous := s.lastStatus
	defer s.reconcileStatus(previous)
	s.teardownPending = s.anyChildStarted()
	if s.lifecycle != ProcessorSupervisorStopped {
		s.lifecycle = ProcessorSupervisorStarting
	}
	s.labels.Count(s, metricProcessorChildTerminations, role, telemetry.TerminationReason(reason))
	return nil
}

// HandleCall returns unsupported calls as responses without terminating the supervisor.
func (s *processorSupervisor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("processor supervisor: unsupported call %T", request), nil
}

// Terminate releases supervisor resources, keeping each child's own diagnostics.
func (s *processorSupervisor) Terminate(reason error) {
	previous := s.lastStatus
	s.err = reason
	s.lifecycle = ProcessorSupervisorStopped
	s.reader.pid, s.reader.status.Lifecycle = gen.PID{}, KafkaReaderStopped
	s.writer.pid, s.writer.status.lifecycle = gen.PID{}, KafkaWriterStopped
	s.dlqWriter.pid, s.dlqWriter.status.lifecycle = gen.PID{}, KafkaWriterStopped
	s.coordinator.pid, s.coordinator.status.Lifecycle = gen.PID{}, CoordinatorStopped
	s.jobPool.pid, s.jobPool.status.lifecycle = gen.PID{}, JobPoolStopped
	s.reader.status.Availability = runtime.AvailabilityUnavailable
	s.writer.status.availability = runtime.AvailabilityUnavailable
	s.dlqWriter.status.availability = runtime.AvailabilityUnavailable
	s.coordinator.status.Availability = runtime.AvailabilityUnavailable
	s.jobPool.status.availability = runtime.AvailabilityUnavailable
	s.reconcileStatus(previous)
}

// ---------------------------------------------------------------------------
// Children
// ---------------------------------------------------------------------------

// readerName returns the reader child name.
func (s *processorSupervisor) readerName() gen.Atom { return KafkaReaderActorName(s.opts.Namespace) }

// writerName returns the output writer child name.
func (s *processorSupervisor) writerName() gen.Atom {
	return KafkaWriterActorName(s.opts.Namespace, s.opts.Writer.Destination)
}

// dlqWriterName returns the DLQ writer child name.
func (s *processorSupervisor) dlqWriterName() gen.Atom {
	return KafkaWriterActorName(s.opts.Namespace, s.opts.DLQWriter.Destination)
}

// coordinatorName returns the coordinator child name.
func (s *processorSupervisor) coordinatorName() gen.Atom { return CoordinatorName(s.opts.Namespace) }

// jobPoolName returns the job pool child name.
func (s *processorSupervisor) jobPoolName() gen.Atom { return JobPoolName(s.opts.Namespace) }

// childSpecs returns direct children for the processor session, in restart order.
func (s *processorSupervisor) childSpecs() []act.SupervisorChildSpec {
	mailbox := gen.ProcessOptions{MailboxSize: s.opts.MailboxSize}
	reader := s.opts.Reader
	reader.Namespace = s.opts.Namespace
	writer := s.opts.Writer
	writer.Namespace = s.opts.Namespace
	dlqWriter := s.opts.DLQWriter
	dlqWriter.Namespace = s.opts.Namespace
	pool := s.opts.JobPool
	pool.Namespace = s.opts.Namespace
	coordinator := s.opts.Coordinator
	return []act.SupervisorChildSpec{
		{Name: s.readerName(), Options: mailbox, Factory: func() gen.ProcessBehavior { return NewKafkaReader(reader, s.newReader, s.barrier) }},
		{Name: s.writerName(), Options: mailbox, Factory: func() gen.ProcessBehavior { return NewKafkaWriter(writer, s.newWriter, s.barrier) }},
		{Name: s.dlqWriterName(), Options: mailbox, Factory: func() gen.ProcessBehavior { return NewKafkaWriter(dlqWriter, s.newDLQWriter, s.barrier) }},
		{Name: s.coordinatorName(), Options: mailbox, Factory: func() gen.ProcessBehavior { return coordinator() }},
		{Name: s.jobPoolName(), Options: gen.ProcessOptions{MailboxSize: pool.MailboxSize}, Factory: func() gen.ProcessBehavior { return NewJobPool(pool) }},
	}
}

// childSnapshots projects every direct child in child-spec order, which fixes error precedence.
func (s *processorSupervisor) childSnapshots() [processorChildCount]childSnapshot {
	return [processorChildCount]childSnapshot{
		{s.readerName(), s.reader.pid, string(s.reader.status.Lifecycle), s.reader.status.Availability, s.reader.status.Err},
		{s.writerName(), s.writer.pid, string(s.writer.status.lifecycle), s.writer.status.availability, s.writer.status.err},
		{s.dlqWriterName(), s.dlqWriter.pid, string(s.dlqWriter.status.lifecycle), s.dlqWriter.status.availability, s.dlqWriter.status.err},
		{s.coordinatorName(), s.coordinator.pid, string(s.coordinator.status.Lifecycle), s.coordinator.status.Availability, s.coordinator.status.Err},
		{s.jobPoolName(), s.jobPool.pid, string(s.jobPool.status.lifecycle), s.jobPool.status.availability, s.jobPool.status.err},
	}
}

// startedChildren counts direct children with a live incarnation.
func (s *processorSupervisor) startedChildren() int {
	count := 0
	for _, child := range s.childSnapshots() {
		if child.pid != (gen.PID{}) {
			count++
		}
	}
	return count
}

// allChildrenStarted reports whether every direct child has started.
func (s *processorSupervisor) allChildrenStarted() bool {
	return s.startedChildren() == processorChildCount
}

// anyChildStarted reports whether any direct child has started.
func (s *processorSupervisor) anyChildStarted() bool { return s.startedChildren() > 0 }

// isChild trusts callback state when it holds an incarnation and falls back to the
// framework registry only for I/O events queued ahead of the start callback.
func (s *processorSupervisor) isChild(pid, tracked gen.PID, name gen.Atom) bool {
	if pid == (gen.PID{}) {
		return false
	}
	if tracked != (gen.PID{}) {
		return pid == tracked
	}
	for _, current := range s.Children() {
		if current.PID == pid {
			return current.Spec == name
		}
	}
	return false
}

// writerChildName returns which writer child owns a PID, empty when it owns neither.
func (s *processorSupervisor) writerChildName(pid gen.PID) gen.Atom {
	switch {
	case s.isChild(pid, s.writer.pid, s.writerName()):
		return s.writerName()
	case s.isChild(pid, s.dlqWriter.pid, s.dlqWriterName()):
		return s.dlqWriterName()
	default:
		return ""
	}
}

// trackedPID returns the PID currently recorded for a direct child name.
func (s *processorSupervisor) trackedPID(name gen.Atom) gen.PID {
	switch name {
	case s.readerName():
		return s.reader.pid
	case s.writerName():
		return s.writer.pid
	case s.dlqWriterName():
		return s.dlqWriter.pid
	case s.coordinatorName():
		return s.coordinator.pid
	case s.jobPoolName():
		return s.jobPool.pid
	default:
		return gen.PID{}
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// registerIOFence records an I/O completion fence for a child incarnation.
func (s *processorSupervisor) registerIOFence(owner gen.PID, name gen.Atom, alias gen.Alias, completion *runtime.IOBarrier, fences map[gen.Alias]processorIOFence) {
	if alias == (gen.Alias{}) || completion == nil {
		return
	}
	if _, exists := s.readerFences[alias]; exists {
		return
	}
	if _, exists := s.writerFences[alias]; exists {
		return
	}
	fences[alias] = processorIOFence{owner: owner, name: name, completion: completion}
}

// completeIOFence completes a tracked I/O fence.
func (s *processorSupervisor) completeIOFence(from gen.PID, alias gen.Alias, message any, fences map[gen.Alias]processorIOFence) error {
	fence, exists := fences[alias]
	if !exists || fence.owner != from || !fence.completion.Quiesced() {
		return nil
	}
	if err := s.forwardIOStopped(fence, message); err != nil {
		return err
	}
	delete(fences, alias)
	previous := s.lastStatus
	err := s.activateKafkaChildren()
	s.reconcileStatus(previous)
	return err
}

// pollIOCompletions processes completed I/O fences.
func (s *processorSupervisor) pollIOCompletions() error {
	for alias, fence := range s.readerFences {
		if fence.completion.Quiesced() {
			if err := s.completeIOFence(fence.owner, alias, MessageKafkaReaderIOStopped{Alias: alias}, s.readerFences); err != nil {
				return err
			}
		}
	}
	for alias, fence := range s.writerFences {
		if fence.completion.Quiesced() {
			if err := s.completeIOFence(fence.owner, alias, MessageKafkaWriterIOStopped{Alias: alias}, s.writerFences); err != nil {
				return err
			}
		}
	}
	return nil
}

// forwardIOStopped forwards an I/O-stopped message unless a replacement already took the fence's name.
func (s *processorSupervisor) forwardIOStopped(fence processorIOFence, message any) error {
	if !s.isChild(fence.owner, s.trackedPID(fence.name), fence.name) {
		return nil
	}
	if err := s.SendWithPriority(fence.owner, message, gen.MessagePriorityHigh); err != nil && !errors.Is(err, gen.ErrProcessUnknown) && !errors.Is(err, gen.ErrProcessTerminated) {
		return fmt.Errorf("processor supervisor: forward Kafka I/O completion to %s: %w", fence.owner, err)
	}
	return nil
}

// activateKafkaChildren activates the reader and writers together.
func (s *processorSupervisor) activateKafkaChildren() error {
	if s.teardownPending || !s.allChildrenStarted() || len(s.readerFences) != 0 || len(s.writerFences) != 0 {
		return nil
	}
	if err := s.activateKafkaChild(s.readerName(), s.reader.pid, &s.reader.activationSent, MessageKafkaReaderStart{}); err != nil {
		return err
	}
	if err := s.activateKafkaChild(s.writerName(), s.writer.pid, &s.writer.activationSent, MessageKafkaWriterStart{}); err != nil {
		return err
	}
	if err := s.activateKafkaChild(s.dlqWriterName(), s.dlqWriter.pid, &s.dlqWriter.activationSent, MessageKafkaWriterStart{}); err != nil {
		return err
	}
	s.lifecycle = ProcessorSupervisorRunning
	return nil
}

// activateKafkaChild sends one activation message once per child incarnation.
func (s *processorSupervisor) activateKafkaChild(name gen.Atom, pid gen.PID, sent *bool, message any) error {
	if *sent {
		return nil
	}
	if err := s.SendWithPriority(pid, message, gen.MessagePriorityHigh); err != nil {
		return fmt.Errorf("processor supervisor: activate Kafka child %s: %w", name, err)
	}
	*sent = true
	return nil
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// status returns the aggregated processor status.
func (s *processorSupervisor) status() ProcessorSupervisorStatus {
	availability := runtime.AvailabilityUnavailable
	if s.lifecycle == ProcessorSupervisorRunning && s.allChildrenStarted() {
		availability = runtime.AvailabilityReady
		for _, child := range s.childSnapshots() {
			if child.availability == runtime.AvailabilityUnavailable {
				availability = runtime.AvailabilityUnavailable
				break
			}
			if child.availability == runtime.AvailabilityDegraded {
				availability = runtime.AvailabilityDegraded
			}
		}
	}
	return ProcessorSupervisorStatus{
		Lifecycle:    s.lifecycle,
		Availability: availability,
		Err: runtime.FirstError(s.err, s.reader.status.Err, s.writer.status.err,
			s.dlqWriter.status.err, s.coordinator.status.Err, s.jobPool.status.err),
	}
}

// reconcileStatus refreshes gauges on every reconciliation and propagates status only on change.
func (s *processorSupervisor) reconcileStatus(previous ProcessorSupervisorStatus) {
	s.publishGauges()
	s.propagateReadiness()
	next := s.status()
	if sameProcessorSupervisorStatus(previous, next) {
		return
	}
	s.lastStatus = next
	s.propagateStatus(next)
}

// propagateReadiness updates Radar from current subtree health without publishing status messages.
func (s *processorSupervisor) propagateReadiness() {
	s.signal.SetReady(s, s.status().Availability == runtime.AvailabilityReady)
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (s *processorSupervisor) propagateStatus(next ProcessorSupervisorStatus) {
	_ = s.SendWithPriority(s.Parent(), MessageProcessorSupervisorStatusChanged{Status: next}, gen.MessagePriorityHigh)
}

// HandleInspect returns supervisor inspection values.
func (s *processorSupervisor) HandleInspect(gen.PID, ...string) map[string]string {
	status := s.status()
	result := map[string]string{
		"processor:err":              runtime.ErrorText(status.Err),
		"processor:lifecycle":        string(status.Lifecycle),
		"processor:availability":     string(status.Availability),
		"processor:readiness_signal": s.signal.State(),
		"processor:reader_io_fences": fmt.Sprintf("%d", len(s.readerFences)),
		"processor:writer_io_fences": fmt.Sprintf("%d", len(s.writerFences)),
	}
	for _, child := range s.childSnapshots() {
		prefix := "processor:child:" + string(child.name)
		result[prefix] = child.pid.String()
		result[prefix+":lifecycle"] = child.lifecycle
		result[prefix+":availability"] = string(child.availability)
		result[prefix+":err"] = runtime.ErrorText(child.err)
	}
	return result
}

// reconcileRadar registers whatever radar is still missing, then heartbeats the readiness signal.
func (s *processorSupervisor) reconcileRadar() {
	if !s.collectorsRegistered {
		if err := telemetry.Register(s.Node(), stageMetricSpecs); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
		if !s.watchRadar(telemetry.MetricsProcess) {
			return
		}
		s.collectorsRegistered = true
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
func (s *processorSupervisor) watchRadar(name gen.Atom) bool {
	if err := s.MonitorProcessID(gen.ProcessID{Name: name, Node: s.Node().Name()}); err != nil && !errors.Is(err, gen.ErrTargetExist) {
		s.Log().Debug("radar monitor unavailable: namespace=%q process=%s error=%v", s.opts.Namespace, name, err)
		return false
	}
	return true
}

// radarUnavailableOnce logs only the first failure of an outage.
func (s *processorSupervisor) radarUnavailableOnce(err error) {
	if s.radarLogged {
		return
	}
	s.radarLogged = true
	s.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", s.opts.Namespace, err)
}

// requestChildStatuses requests status from all direct children.
func (s *processorSupervisor) requestChildStatuses() {
	for _, child := range s.childSnapshots() {
		s.requestChildStatus(child.name, child.pid)
	}
}

// requestChildStatus requests status from one child.
func (s *processorSupervisor) requestChildStatus(name gen.Atom, pid gen.PID) {
	if pid == (gen.PID{}) {
		return
	}
	var message any
	switch name {
	case s.readerName():
		message = MessageKafkaReaderStatusRequest{}
	case s.coordinatorName():
		message = MessageCoordinatorStatusRequest{}
	case s.jobPoolName():
		message = MessageJobPoolStatusRequest{}
	default:
		message = MessageKafkaWriterStatusRequest{}
	}
	_ = s.SendWithPriority(pid, message, gen.MessagePriorityHigh)
}

// publishGauges publishes processor supervisor gauges.
func (s *processorSupervisor) publishGauges() {
	processorSupervisorGauges{availability: s.status().Availability, children: s.startedChildren()}.publish(s.labels, s)
}

// sameProcessorSupervisorStatus compares the status fields that trigger publication.
func sameProcessorSupervisorStatus(left, right ProcessorSupervisorStatus) bool {
	return left.Lifecycle == right.Lifecycle && left.Availability == right.Availability && runtime.ErrorText(left.Err) == runtime.ErrorText(right.Err)
}

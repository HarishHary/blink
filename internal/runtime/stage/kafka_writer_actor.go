package stage

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// kafkaWriterMetaState tracks the writer actor's meta-process state.
type kafkaWriterMetaState struct {
	alias               gen.Alias
	activeIO            map[gen.Alias]struct{}
	replacementPending  bool
	consecutiveFailures int
	restart             *runtime.ScheduledBackoff
	status              kafkaWriterMetaStatus
}

// KafkaWriterLifecycle describes a Kafka writer lifecycle state.
type KafkaWriterLifecycle string

const (
	KafkaWriterStarting KafkaWriterLifecycle = "starting"
	KafkaWriterRunning  KafkaWriterLifecycle = "running"
	KafkaWriterDraining KafkaWriterLifecycle = "draining"
	KafkaWriterDrained  KafkaWriterLifecycle = "drained"
	KafkaWriterStopped  KafkaWriterLifecycle = "stopped"
)

// kafkaWriterStatus reports Kafka writer activity and actor status.
type kafkaWriterStatus struct {
	lifecycle    KafkaWriterLifecycle
	availability runtime.Availability
	err          error
	writing      bool
}

// kafkaWriterPublication holds one accepted publication until completion.
type kafkaWriterPublication struct {
	from        gen.PID
	message     MessagePublishRecords
	records     []brokers.Message
	bytes       int
	operationID gen.Ref
}

// kafkaWriterActor serializes Kafka publication admission and completion.
type kafkaWriterActor struct {
	act.Actor
	opts            KafkaWriterActorOptions
	newWriter       func() brokers.Writer
	barrier         *runtime.IOBarrier
	lifecycle       KafkaWriterLifecycle
	meta            kafkaWriterMetaState
	active          *kafkaWriterPublication
	queue           []kafkaWriterPublication
	err             error
	lastStatus      kafkaWriterStatus
	lastStatusEpoch int64
	labels          telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessagePublishRecords requests one Kafka-acknowledged publication.
type MessagePublishRecords struct {
	Session       gen.PID
	PublicationID gen.Ref
	Attempt       uint64
	Records       []brokers.Message
}

// MessagePublishResult reports a final publication outcome to the original sender.
type MessagePublishResult struct {
	Session       gen.PID
	PublicationID gen.Ref
	Attempt       uint64
	Err           error
	Ambiguous     bool
}

// MessageDrainWriter stops admissions until accepted work completes.
type MessageDrainWriter struct{}

// MessageKafkaWriterStatusRequest requests a Kafka writer's current status.
type MessageKafkaWriterStatusRequest struct{}

// MessageKafkaWriterStatusChanged reports a Kafka writer's current status.
type MessageKafkaWriterStatusChanged struct {
	statusEpoch int64
	status      kafkaWriterStatus
}

// kafkaWriterStatusRequest asks for the current writer status.
type kafkaWriterStatusRequest struct{}

// kafkaWriterStatusResponse reports writer lifecycle, availability, and active I/O.
type kafkaWriterStatusResponse struct {
	status kafkaWriterStatus
}

// MessageKafkaWriterStart starts the writer meta-process.
type MessageKafkaWriterStart struct{}

// MessageKafkaWriterRestart triggers a fenced meta-process restart.
type MessageKafkaWriterRestart struct{ token uint64 }

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// NewKafkaWriter creates a writer actor with a fresh lazy writer per meta-process.
func NewKafkaWriter(opts KafkaWriterActorOptions, newWriter func() brokers.Writer, barrier *runtime.IOBarrier) gen.ProcessBehavior {
	opts = kafkaWriterActorOptionsWithDefaults(opts)
	return &kafkaWriterActor{opts: opts, newWriter: newWriter, barrier: barrier, labels: telemetry.NewLabels(opts.Namespace, opts.Destination)}
}

// Init validates and initializes the writer actor state.
func (a *kafkaWriterActor) Init(...any) error {
	if a.newWriter == nil {
		return fmt.Errorf("kafka writer: writer factory is required")
	}
	if a.barrier == nil {
		return fmt.Errorf("kafka writer: I/O barrier is required")
	}
	if err := validateKafkaWriterOptions(a.opts); err != nil {
		return err
	}
	a.lifecycle = KafkaWriterStarting
	a.meta = kafkaWriterMetaState{
		activeIO: make(map[gen.Alias]struct{}),
		restart:  runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		status: kafkaWriterMetaStatus{
			lifecycle:    KafkaWriterMetaStarting,
			availability: runtime.AvailabilityUnavailable,
		},
	}
	a.publishGauges()
	return nil
}

// HandleMessage handles lifecycle, publication, and meta-process messages.
func (a *kafkaWriterActor) HandleMessage(from gen.PID, message any) (err error) {
	defer func() {
		if err != nil {
			a.err = err
		}
		a.reconcileStatus()
	}()
	switch m := message.(type) {
	case MessageKafkaWriterStatusRequest:
		if from == a.Parent() {
			epoch := a.lastStatusEpoch
			a.reconcileStatus()
			if a.lastStatusEpoch == epoch {
				a.propagateStatus(a.lastStatus)
			}
		}
	case MessageKafkaWriterStart:
		if from == a.Parent() && a.lifecycle == KafkaWriterStarting && a.meta.alias == (gen.Alias{}) && !a.meta.replacementPending {
			a.meta.replacementPending = true
			return a.startMeta()
		}
	case MessageKafkaWriterRestart:
		if from != a.PID() || !a.meta.restart.Pending || m.token != a.meta.restart.Token || !a.meta.replacementPending || a.meta.alias != (gen.Alias{}) || len(a.meta.activeIO) != 0 || !a.canRestartMeta() {
			return nil
		}
		a.meta.restart.Pending = false
		a.meta.restart.Cancel = nil
		return a.startMeta()
	case MessageKafkaWriterReady:
		if from != a.PID() || m.alias != a.meta.alias || !a.canRestartMeta() {
			return nil
		}
		a.meta.replacementPending = false
		a.meta.status.lifecycle = KafkaWriterMetaRunning
		switch {
		case a.meta.consecutiveFailures >= kafkaWriterUnavailableThreshold:
			a.meta.status.availability = runtime.AvailabilityUnavailable
		case a.meta.consecutiveFailures > 0:
			a.meta.status.availability = runtime.AvailabilityDegraded
		default:
			a.meta.status.availability = runtime.AvailabilityReady
		}
		a.meta.status.err = nil
		if a.lifecycle == KafkaWriterStarting {
			a.lifecycle = KafkaWriterRunning
		}
		a.err = nil
		return a.dispatchWrite()
	case MessagePublishRecords:
		if from.Node != a.PID().Node {
			return a.rejectPublication(from, m, fmt.Errorf("kafka writer: sender must be local"), false)
		}
		if err := validatePublication(from, m, a.opts); err != nil {
			return a.rejectPublication(from, m, err, false)
		}
		if a.lifecycle == KafkaWriterDraining || a.lifecycle == KafkaWriterDrained {
			return a.rejectPublication(from, m, fmt.Errorf("kafka writer: draining"), false)
		}
		if a.lifecycle != KafkaWriterRunning || a.meta.status.lifecycle != KafkaWriterMetaRunning {
			return a.rejectPublication(from, m, fmt.Errorf("kafka writer: not ready"), false)
		}
		if a.active != nil && samePublication(a.active.message, m) {
			return nil
		}
		for _, pending := range a.queue {
			if samePublication(pending.message, m) {
				return nil
			}
		}
		publications, _, _ := a.pendingSize()
		if publications >= a.opts.MaxPending {
			return a.rejectPublication(from, m, fmt.Errorf("kafka writer: pending publication limit reached"), false)
		}
		publication := kafkaWriterPublication{
			from: from, message: MessagePublishRecords{
				Session: m.Session, PublicationID: m.PublicationID, Attempt: m.Attempt,
			},
			records: brokers.CloneMessages(m.Records),
			bytes:   brokers.TotalBytes(m.Records),
		}
		if a.active == nil {
			a.active = &publication
			if err := a.dispatchWrite(); err != nil {
				return err
			}
		} else {
			a.queue = append(a.queue, publication)
		}
		return nil
	case MessageDrainWriter:
		if from != a.Parent() || (a.lifecycle != KafkaWriterStarting && a.lifecycle != KafkaWriterRunning) {
			return nil
		}
		a.lifecycle = KafkaWriterDraining
		a.meta.status.availability = runtime.AvailabilityUnavailable
		if a.active == nil {
			a.meta.replacementPending = false
			a.meta.restart.CancelScheduled(false)
		}
		return a.maybeDrained()
	case MessageKafkaWriterWriteResult:
		_, active := a.meta.activeIO[m.source]
		if from != a.PID() || a.active == nil || !active || m.operationID != a.active.operationID {
			return nil
		}
		if m.ambiguous {
			if m.err != nil {
				a.recordBrokerResult(m.err)
			}
			a.stopMeta()
			return nil
		}
		publication := a.active
		a.active = nil
		result := MessagePublishResult{
			Session: publication.message.Session, PublicationID: publication.message.PublicationID, Attempt: publication.message.Attempt,
			Err: m.err, Ambiguous: m.ambiguous,
		}
		a.recordBrokerResult(m.err)
		if err := a.Send(publication.from, result); err != nil {
			return fmt.Errorf("kafka writer: reply publication result: %w", err)
		}
		if len(a.queue) != 0 {
			next := a.queue[0]
			a.queue[0] = kafkaWriterPublication{}
			a.queue = a.queue[1:]
			a.active = &next
			return a.dispatchWrite()
		}
		return a.maybeDrained()
	case MessageKafkaWriterRetryProgress:
		_, active := a.meta.activeIO[m.source]
		if from == a.PID() && a.active != nil && active && m.operationID == a.active.operationID && m.err != nil {
			a.recordBrokerResult(fmt.Errorf("kafka writer operation retry: %w", m.err))
		}
	case MessageKafkaWriterIOStopped:
		if from != a.Parent() {
			return nil
		}
		if _, ok := a.meta.activeIO[m.Alias]; !ok {
			return nil
		}
		if m.Alias == a.meta.alias {
			a.metaLost(m.Alias, errors.New("kafka writer meta stopped without down notification"))
		}
		delete(a.meta.activeIO, m.Alias)
		if len(a.meta.activeIO) != 0 || a.meta.alias != (gen.Alias{}) {
			return nil
		}
		if a.lifecycle == KafkaWriterDraining && a.active == nil {
			a.meta.replacementPending = false
			return a.maybeDrained()
		}
		return a.scheduleMetaRestart()
	case gen.MessageDownAlias:
		if m.Alias != a.meta.alias {
			return nil
		}
		reason := m.Reason
		if reason == nil {
			reason = errors.New("unknown termination reason")
		}
		a.metaLost(m.Alias, fmt.Errorf("kafka writer meta stopped: %w", reason))
		return nil
	}
	return nil
}

// HandleCall returns the current writer status for local callers.
func (a *kafkaWriterActor) HandleCall(from gen.PID, _ gen.Ref, request any) (any, error) {
	if _, ok := request.(kafkaWriterStatusRequest); ok {
		if from.Node != a.PID().Node {
			return gen.ErrNotAllowed, nil
		}
		return kafkaWriterStatusResponse{status: a.status()}, nil
	}
	return fmt.Errorf("kafka writer: unsupported call %T", request), nil
}

// Terminate stops the writer actor and its active meta-process.
func (a *kafkaWriterActor) Terminate(reason error) {
	a.err = runtime.FirstError(reason, a.err)
	a.lifecycle = KafkaWriterStopped
	a.meta.status.lifecycle = KafkaWriterMetaStopped
	a.meta.status.availability = runtime.AvailabilityUnavailable
	a.meta.restart.CancelScheduled(false)
	if a.meta.alias != (gen.Alias{}) {
		_ = a.SendExitMeta(a.meta.alias, gen.TerminateReasonShutdown)
	}
	a.reconcileStatus()
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// recordBrokerResult updates writer health from one broker result.
func (a *kafkaWriterActor) recordBrokerResult(err error) {
	if err != nil {
		a.meta.consecutiveFailures++
		a.meta.status.err = err
		if a.meta.status.lifecycle != KafkaWriterMetaRunning || a.meta.consecutiveFailures >= kafkaWriterUnavailableThreshold {
			a.meta.status.availability = runtime.AvailabilityUnavailable
			return
		}
		a.meta.status.availability = runtime.AvailabilityDegraded
		return
	}
	a.meta.consecutiveFailures = 0
	if a.meta.status.lifecycle == KafkaWriterMetaRunning {
		a.meta.status.availability = runtime.AvailabilityReady
		a.meta.status.err = nil
	}
	a.meta.restart.CancelScheduled(true)
}

// dispatchWrite sends the active publication to the running meta-process.
func (a *kafkaWriterActor) dispatchWrite() error {
	if a.active == nil || a.meta.alias == (gen.Alias{}) || a.meta.status.lifecycle != KafkaWriterMetaRunning {
		return nil
	}
	a.active.operationID = a.Node().MakeRef()
	if err := a.Send(a.meta.alias, MessageKafkaWriterWrite{operationID: a.active.operationID, records: brokers.CloneMessages(a.active.records)}); err != nil {
		a.meta.status.err = fmt.Errorf("kafka writer: dispatch publication: %w", err)
		a.stopMeta()
		return nil
	}
	return nil
}

// maybeDrained marks the actor drained after all accepted work completes.
func (a *kafkaWriterActor) maybeDrained() error {
	if a.lifecycle != KafkaWriterDraining || a.active != nil || len(a.queue) != 0 {
		return nil
	}
	a.lifecycle = KafkaWriterDrained
	a.meta.replacementPending = false
	a.meta.restart.CancelScheduled(false)
	return nil
}

// rejectPublication sends a failed result for a rejected publication.
func (a *kafkaWriterActor) rejectPublication(from gen.PID, message MessagePublishRecords, err error, ambiguous bool) error {
	a.labels.Count(a, metricKafkaWriterQueueRejects)
	return a.Send(from, MessagePublishResult{
		Session: message.Session, PublicationID: message.PublicationID, Attempt: message.Attempt, Err: err, Ambiguous: ambiguous,
	})
}

// pendingSize returns queued publication, record, and byte counts.
func (a *kafkaWriterActor) pendingSize() (publications, records, bytes int) {
	if a.active != nil {
		publications, records, bytes = 1, len(a.active.records), a.active.bytes
	}
	for _, publication := range a.queue {
		publications++
		records += len(publication.records)
		bytes += publication.bytes
	}
	return publications, records, bytes
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// startMeta starts a replacement meta-process when permitted.
func (a *kafkaWriterActor) startMeta() error {
	if !a.canRestartMeta() || !a.meta.replacementPending || a.meta.alias != (gen.Alias{}) || len(a.meta.activeIO) != 0 {
		return nil
	}
	a.meta.status = kafkaWriterMetaStatus{
		lifecycle:    KafkaWriterMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		err:          a.meta.status.err,
	}
	alias, err := a.SpawnMeta(&kafkaWriterMeta{
		newWriter: a.newWriter, barrier: a.barrier, writeTimeout: a.opts.WriteTimeout,
		retryMin: a.opts.RetryMin, retryMax: a.opts.RetryMax, supervisor: a.Parent(), labels: a.labels,
	}, gen.MetaOptions{MailboxSize: 1})
	if err != nil {
		a.meta.status.err = fmt.Errorf("kafka writer: spawn meta: %w", err)
		return a.scheduleMetaRestart()
	}
	a.meta.activeIO[alias] = struct{}{}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.meta.status.err = fmt.Errorf("kafka writer: monitor meta: %w", err)
		return nil
	}
	a.meta.alias = alias
	a.meta.replacementPending = false
	return nil
}

// scheduleMetaRestart schedules a replacement after active I/O stops.
func (a *kafkaWriterActor) scheduleMetaRestart() error {
	if !a.canRestartMeta() || !a.meta.replacementPending || a.meta.alias != (gen.Alias{}) || len(a.meta.activeIO) != 0 || a.meta.restart.Pending {
		return nil
	}
	delay := a.meta.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		err := fmt.Errorf("kafka writer meta restart: %w", runtime.ErrBackoffStopped)
		if a.active != nil {
			if sendErr := a.Send(a.active.from, MessagePublishResult{
				Session: a.active.message.Session, PublicationID: a.active.message.PublicationID,
				Attempt: a.active.message.Attempt, Err: err, Ambiguous: true,
			}); sendErr != nil {
				return fmt.Errorf("kafka writer: report exhausted publication recovery: %w", sendErr)
			}
		}
		return err
	}
	a.meta.restart.Token++
	token := a.meta.restart.Token
	cancel, err := a.SendWithPriorityAfter(a.PID(), MessageKafkaWriterRestart{token: token}, gen.MessagePriorityHigh, delay)
	if err != nil {
		return fmt.Errorf("kafka writer: schedule meta restart: %w", err)
	}
	a.meta.restart.Pending = true
	a.meta.restart.Cancel = cancel
	a.meta.status.lifecycle = KafkaWriterMetaRestarting
	a.meta.status.availability = runtime.AvailabilityUnavailable
	return nil
}

// canRestartMeta reports whether the current lifecycle permits replacement.
func (a *kafkaWriterActor) canRestartMeta() bool {
	if a.lifecycle == KafkaWriterStarting || a.lifecycle == KafkaWriterRunning {
		return true
	}
	return a.lifecycle == KafkaWriterDraining && a.active != nil
}

// metaLost records the active meta-process loss and replacement need.
func (a *kafkaWriterActor) metaLost(alias gen.Alias, reason error) {
	if alias == a.meta.alias {
		a.meta.alias = gen.Alias{}
		_ = a.DemonitorAlias(alias)
	}
	a.meta.replacementPending = true
	a.meta.status.lifecycle = KafkaWriterMetaRestarting
	a.meta.status.availability = runtime.AvailabilityUnavailable
	a.meta.status.err = reason
}

// stopMeta requests shutdown of the active meta-process.
func (a *kafkaWriterActor) stopMeta() {
	if a.meta.alias == (gen.Alias{}) {
		return
	}
	alias := a.meta.alias
	reason := a.meta.status.err
	if reason == nil {
		reason = errors.New("kafka writer meta replacement requested")
	}
	a.metaLost(alias, reason)
	_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
}

// validatePublication validates a publication against writer limits.
func validatePublication(from gen.PID, message MessagePublishRecords, opts KafkaWriterActorOptions) error {
	if from == (gen.PID{}) || message.Session == (gen.PID{}) || message.PublicationID == (gen.Ref{}) || message.Attempt == 0 {
		return fmt.Errorf("kafka writer: sender, session, publication ID, and attempt are required")
	}
	if len(message.Records) == 0 {
		return fmt.Errorf("kafka writer: at least one record is required")
	}
	if len(message.Records) > opts.MaxRecords {
		return fmt.Errorf("kafka writer: record limit exceeded")
	}
	if brokers.TotalBytes(message.Records) > opts.MaxBytes {
		return fmt.Errorf("kafka writer: byte limit exceeded")
	}
	return nil
}

// samePublication reports whether two publication identities match.
func samePublication(a, b MessagePublishRecords) bool {
	return a.Session == b.Session && a.PublicationID == b.PublicationID && a.Attempt == b.Attempt
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// status returns the current writer status.
func (a *kafkaWriterActor) status() kafkaWriterStatus {
	status := kafkaWriterStatus{
		lifecycle:    a.lifecycle,
		availability: runtime.AvailabilityUnavailable,
		writing:      a.active != nil && a.lifecycle != KafkaWriterStopped && a.lifecycle != KafkaWriterDrained,
	}
	if a.lifecycle == KafkaWriterRunning {
		status.availability = a.meta.status.availability
		if status.availability == runtime.AvailabilityReady && a.err != nil {
			status.availability = runtime.AvailabilityDegraded
		}
	}
	// The actor's own failure wins over its meta's.
	status.err = runtime.FirstError(a.err, a.meta.status.err)
	return status
}

// publishGauges publishes the writer queue gauges.
func (a *kafkaWriterActor) publishGauges() {
	publications, _, bytes := a.pendingSize()
	kafkaWriterActorGauges{
		availability: a.status().availability,
		pending:      publications,
		pendingBytes: bytes,
	}.publish(a.labels, a)
}

// reconcileStatus refreshes gauges on every reconciliation and propagates status only on change.
func (a *kafkaWriterActor) reconcileStatus() {
	a.publishGauges()
	next := a.status()
	if sameKafkaWriterStatus(a.lastStatus, next) {
		return
	}
	a.lastStatusEpoch = runtime.NextStatusEpoch(a.lastStatusEpoch)
	a.lastStatus = next
	a.propagateStatus(next)
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (a *kafkaWriterActor) propagateStatus(next kafkaWriterStatus) {
	_ = a.SendWithPriority(a.Parent(), MessageKafkaWriterStatusChanged{statusEpoch: a.lastStatusEpoch, status: next}, gen.MessagePriorityHigh)
}

// HandleInspect exposes lifecycle, fencing identities, and immutable queue counters.
func (a *kafkaWriterActor) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	publications, records, bytes := a.pendingSize()
	owner := gen.PID{}
	if a.active != nil {
		owner = a.active.from
	}
	return map[string]string{
		"kafka_writer:lifecycle":                 string(status.lifecycle),
		"kafka_writer:availability":              string(status.availability),
		"kafka_writer:err":                       runtime.ErrorText(status.err),
		"kafka_writer:session":                   a.PID().String(),
		"kafka_writer:meta:alias":                a.meta.alias.String(),
		"kafka_writer:meta:lifecycle":            string(a.meta.status.lifecycle),
		"kafka_writer:meta:availability":         string(a.meta.status.availability),
		"kafka_writer:meta:err":                  runtime.ErrorText(a.meta.status.err),
		"kafka_writer:writing":                   fmt.Sprintf("%t", status.writing),
		"kafka_writer:meta:active_io":            fmt.Sprintf("%d", len(a.meta.activeIO)),
		"kafka_writer:meta:replacement":          fmt.Sprintf("%t", a.meta.replacementPending),
		"kafka_writer:meta:consecutive_failures": fmt.Sprintf("%d", a.meta.consecutiveFailures),
		"kafka_writer:meta:restart_pending":      fmt.Sprintf("%t", a.meta.restart.Pending),
		"kafka_writer:owner":                     owner.String(),
		"kafka_writer:queue":                     fmt.Sprintf("%d", len(a.queue)),
		"kafka_writer:pending":                   fmt.Sprintf("%d", publications),
		"kafka_writer:records":                   fmt.Sprintf("%d", records),
		"kafka_writer:bytes":                     fmt.Sprintf("%d", bytes),
	}
}

// sameKafkaWriterStatus compares the status fields that trigger publication.
func sameKafkaWriterStatus(left, right kafkaWriterStatus) bool {
	return left.lifecycle == right.lifecycle &&
		left.availability == right.availability &&
		runtime.ErrorText(left.err) == runtime.ErrorText(right.err) &&
		left.writing == right.writing
}

package stage

import (
	"errors"
	"fmt"
	"slices"

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

var (
	errKafkaReaderProtocol = errors.New("kafka reader protocol violation")
	errKafkaReaderCapacity = errors.New("kafka reader capacity exceeded")
)

// kafkaReaderOperationKind identifies the active reader operation.
type kafkaReaderOperationKind uint8

const (
	kafkaReaderIdle kafkaReaderOperationKind = iota
	kafkaReaderFetching
	kafkaReaderCommitting
)

// KafkaReaderLifecycle describes a Kafka reader lifecycle state.
type KafkaReaderLifecycle string

const (
	KafkaReaderStarting KafkaReaderLifecycle = "starting"
	KafkaReaderRunning  KafkaReaderLifecycle = "running"
	KafkaReaderDraining KafkaReaderLifecycle = "draining"
	KafkaReaderDrained  KafkaReaderLifecycle = "drained"
	KafkaReaderStopped  KafkaReaderLifecycle = "stopped"
)

// KafkaReaderStatus reports Kafka reader activity and actor status. It is exported because the reader
// sends it to the caller-supplied coordinator.
type KafkaReaderStatus struct {
	Lifecycle    KafkaReaderLifecycle
	Availability runtime.Availability
	Err          error
	Fetching     bool
	Committing   bool
}

// RecordRef identifies one fetched broker record.
type RecordRef struct {
	Topic     string
	Partition int
	Offset    int64
}

// kafkaReaderOperation tracks one active fetch or commit.
type kafkaReaderOperation struct {
	kind         kafkaReaderOperationKind
	id           gen.Ref
	fetchLimit   int
	commitRefs   []RecordRef
	commitCounts map[kafkaReaderPartition]int
}

// kafkaReaderPartition identifies a broker partition.
type kafkaReaderPartition struct {
	topic     string
	partition int
}

// kafkaReaderLedgerEntry tracks one fetched record until commitment.
type kafkaReaderLedgerEntry struct {
	ref       RecordRef
	byteCount int
	resolved  bool
}

// kafkaReaderLedger retains uncommitted records and fetched offset floors.
type kafkaReaderLedger struct {
	entriesByPartition map[kafkaReaderPartition][]*kafkaReaderLedgerEntry
	entriesByRef       map[RecordRef]*kafkaReaderLedgerEntry
	lastFetchedOffsets map[kafkaReaderPartition]int64
	partitionOrder     []kafkaReaderPartition
	pendingBytes       int
}

// kafkaReaderMetaState tracks the reader meta lifecycle and restart state.
type kafkaReaderMetaState struct {
	alias               gen.Alias
	replacementPending  bool
	consecutiveFailures int
	activeIO            map[gen.Alias]struct{}
	restart             *runtime.ScheduledBackoff
	status              kafkaReaderMetaStatus
}

// kafkaReaderActor owns one session-wide uncommitted ledger and reader meta.
type kafkaReaderActor struct {
	act.Actor
	coordinator     gen.PID
	opts            KafkaReaderActorOptions
	newReader       func() brokers.Reader
	barrier         *runtime.IOBarrier
	lifecycle       KafkaReaderLifecycle
	meta            kafkaReaderMetaState
	ledger          kafkaReaderLedger
	operation       kafkaReaderOperation
	err             error
	lastStatus      KafkaReaderStatus
	lastStatusEpoch int64
	labels          telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageFetchCredit grants capacity for at most Records new records.
type MessageFetchCredit struct {
	Records int
}

// MessageBatchFetched returns one broker fetch; Records are deep-cloned for the receiver.
type MessageBatchFetched struct {
	BatchID gen.Ref
	Records []brokers.Message
	Err     error
}

// MessageSourceResolved marks complete source obligations. Only the bound local coordinator may send it.
type MessageSourceResolved struct {
	Sources []RecordRef
}

// MessageCommitResult reports the exact ledger prefix covered by one commit attempt.
type MessageCommitResult struct {
	OperationID gen.Ref
	Sources     []RecordRef
	Err         error
}

// MessageDrainReader stops credit and commits only resolved records when sent to the exact reader PID.
type MessageDrainReader struct{}

// MessageKafkaReaderStatusRequest requests a Kafka reader's current status.
type MessageKafkaReaderStatusRequest struct{}

// MessageKafkaReaderStatusChanged reports a Kafka reader's current status.
type MessageKafkaReaderStatusChanged struct {
	StatusEpoch int64
	Status      KafkaReaderStatus
}

// KafkaReaderStatusRequest asks for the current actor-incarnation session identity.
type KafkaReaderStatusRequest struct{}

// KafkaReaderStatusResponse reports the fenced session and current ledger state.
type KafkaReaderStatusResponse struct {
	Session gen.PID
	Status  KafkaReaderStatus
	Records int
	Bytes   int
}

// MessageKafkaReaderStart requests initial reader meta startup.
type MessageKafkaReaderStart struct{}

// MessageKafkaReaderRestart requests a fenced reader meta restart.
type MessageKafkaReaderRestart struct{ token uint64 }

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// NewKafkaReader creates a local reader session with a fresh lazy client factory.
func NewKafkaReader(opts KafkaReaderActorOptions, newReader func() brokers.Reader, barrier *runtime.IOBarrier) gen.ProcessBehavior {
	opts = kafkaReaderActorOptionsWithDefaults(opts)
	return &kafkaReaderActor{opts: opts, newReader: newReader, barrier: barrier}
}

// Init validates hard ledger bounds and publishes initial gauges. The parent activates initial I/O.
func (a *kafkaReaderActor) Init(...any) error {
	if a.newReader == nil {
		return fmt.Errorf("kafka reader: reader factory is required")
	}
	if a.barrier == nil {
		return fmt.Errorf("kafka reader: I/O barrier is required")
	}
	if err := validateKafkaReaderOptions(a.opts); err != nil {
		return err
	}
	a.lifecycle = KafkaReaderStarting
	a.meta = kafkaReaderMetaState{
		activeIO: make(map[gen.Alias]struct{}),
		restart:  runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		status: kafkaReaderMetaStatus{
			lifecycle:    KafkaReaderMetaStarting,
			availability: runtime.AvailabilityUnavailable,
		},
	}
	a.labels = telemetry.NewLabels(a.opts.Namespace)
	a.ledger = kafkaReaderLedger{
		entriesByPartition: make(map[kafkaReaderPartition][]*kafkaReaderLedgerEntry),
		entriesByRef:       make(map[RecordRef]*kafkaReaderLedgerEntry),
		lastFetchedOffsets: make(map[kafkaReaderPartition]int64),
	}
	a.publishGauges()
	return nil
}

// HandleMessage applies sender fencing before changing ledger or broker state.
func (a *kafkaReaderActor) HandleMessage(from gen.PID, message any) (err error) {
	defer func() {
		if err != nil {
			a.err = err
		}
		a.reconcileStatus()
	}()
	switch message := message.(type) {
	case MessageKafkaReaderStatusRequest:
		if from == a.Parent() {
			epoch := a.lastStatusEpoch
			a.reconcileStatus()
			if a.lastStatusEpoch == epoch {
				a.propagateStatus(a.lastStatus)
			}
		}
	case MessageKafkaReaderStart:
		if from == a.Parent() && a.lifecycle == KafkaReaderStarting && a.meta.alias == (gen.Alias{}) && !a.meta.replacementPending {
			a.meta.replacementPending = true
			return a.startMeta()
		}
	case MessageKafkaReaderRestart:
		if from != a.PID() || !a.meta.restart.Pending || message.token != a.meta.restart.Token || !a.meta.replacementPending || a.meta.alias != (gen.Alias{}) || len(a.meta.activeIO) != 0 || !a.canRestartMeta() {
			return nil
		}
		a.meta.restart.Pending = false
		a.meta.restart.Cancel = nil
		return a.startMeta()
	case MessageKafkaReaderReady:
		if from != a.PID() || message.alias != a.meta.alias || !a.canRestartMeta() {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_meta_ready")
			return nil
		}
		a.meta.replacementPending = false
		a.meta.status.lifecycle = KafkaReaderMetaRunning
		switch {
		case a.meta.consecutiveFailures >= kafkaReaderUnavailableThreshold:
			a.meta.status.availability = runtime.AvailabilityUnavailable
		case a.meta.consecutiveFailures > 0:
			a.meta.status.availability = runtime.AvailabilityDegraded
		default:
			a.meta.status.availability = runtime.AvailabilityReady
		}
		a.meta.status.err = nil
		if a.lifecycle == KafkaReaderStarting {
			a.lifecycle = KafkaReaderRunning
		}
		a.err = nil
		if a.operation.kind == kafkaReaderFetching {
			return a.dispatchFetch()
		}
	case MessageFetchCredit:
		if from.Node != a.PID().Node {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_or_foreign_credit")
			if err := a.Send(from, MessageBatchFetched{Err: gen.ErrNotAllowed}); err != nil {
				return fmt.Errorf("kafka reader: report fetch: %w", err)
			}
			return nil
		}
		if message.Records <= 0 {
			a.labels.Count(a, metricKafkaReaderRejections, "invalid_credit")
			if err := a.Send(from, MessageBatchFetched{Err: gen.ErrIncorrect}); err != nil {
				return fmt.Errorf("kafka reader: report fetch: %w", err)
			}
			return nil
		}
		if a.lifecycle != KafkaReaderRunning || a.meta.status.lifecycle != KafkaReaderMetaRunning {
			a.labels.Count(a, metricKafkaReaderRejections, "not_ready")
			if err := a.Send(from, MessageBatchFetched{Err: gen.ErrBusy}); err != nil {
				return fmt.Errorf("kafka reader: report fetch: %w", err)
			}
			return nil
		}
		if a.coordinator != (gen.PID{}) && from != a.coordinator {
			a.labels.Count(a, metricKafkaReaderRejections, "foreign_coordinator")
			if err := a.Send(from, MessageBatchFetched{Err: gen.ErrNotAllowed}); err != nil {
				return fmt.Errorf("kafka reader: report fetch: %w", err)
			}
			return nil
		}
		if a.operation.kind != kafkaReaderIdle || len(a.ledger.entriesByRef) >= a.opts.MaxRecords || a.ledger.pendingBytes >= a.opts.MaxPendingBytes {
			a.labels.Count(a, metricKafkaReaderRejections, "busy")
			if err := a.Send(from, MessageBatchFetched{Err: gen.ErrBusy}); err != nil {
				return fmt.Errorf("kafka reader: report fetch: %w", err)
			}
			return nil
		}
		if a.coordinator == (gen.PID{}) {
			if err := a.MonitorPID(from); err != nil {
				return fmt.Errorf("kafka reader: monitor coordinator: %w", err)
			}
			a.coordinator = from
		}
		limit := min(message.Records, a.opts.MaxRecords-len(a.ledger.entriesByRef))
		a.operation = kafkaReaderOperation{kind: kafkaReaderFetching, fetchLimit: limit}
		return a.dispatchFetch()
	case MessageSourceResolved:
		if a.coordinator == (gen.PID{}) || from != a.coordinator || from.Node != a.PID().Node {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_or_foreign_resolution")
			return nil
		}
		seen := make(map[RecordRef]struct{}, len(message.Sources))
		for _, ref := range message.Sources {
			if _, duplicate := seen[ref]; duplicate {
				continue
			}
			seen[ref] = struct{}{}
			if a.ledger.entriesByRef[ref] == nil {
				return fmt.Errorf("%w: resolve unknown source %+v", errKafkaReaderProtocol, ref)
			}
		}
		for ref := range seen {
			a.ledger.entriesByRef[ref].resolved = true
		}
		return a.maybeCommitOrDrain()
	case MessageDrainReader:
		if from.Node != a.PID().Node {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_or_foreign_drain")
			return nil
		}
		if a.coordinator == (gen.PID{}) {
			if from != a.Parent() {
				a.labels.Count(a, metricKafkaReaderRejections, "stale_or_foreign_drain")
				return nil
			}
		} else if from != a.coordinator {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_or_foreign_drain")
			return nil
		}
		if a.lifecycle != KafkaReaderStarting && a.lifecycle != KafkaReaderRunning {
			return nil
		}
		if a.coordinator == (gen.PID{}) {
			if err := a.MonitorPID(from); err != nil {
				return fmt.Errorf("kafka reader: monitor drain coordinator: %w", err)
			}
			a.coordinator = from
		}
		a.lifecycle = KafkaReaderDraining
		a.meta.status.availability = runtime.AvailabilityUnavailable
		if a.operation.kind == kafkaReaderIdle && len(a.ledger.entriesByRef) == 0 {
			a.meta.replacementPending = false
			a.meta.restart.CancelScheduled(false)
		}
		return a.maybeCommitOrDrain()
	case MessageKafkaReaderFetchResult:
		_, active := a.meta.activeIO[message.source]
		if from != a.PID() || !active || a.operation.kind != kafkaReaderFetching || message.operationID != a.operation.id {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_fetch_result")
			return nil
		}
		limit := a.operation.fetchLimit
		a.operation = kafkaReaderOperation{}
		if len(message.records) > limit || len(message.records) > a.opts.MaxRecords-len(a.ledger.entriesByRef) {
			return fmt.Errorf("%w: fetched records=%d free=%d", errKafkaReaderCapacity, len(message.records), a.opts.MaxRecords-len(a.ledger.entriesByRef))
		}
		if err := a.acceptFetched(message.records); err != nil {
			return err
		}
		if message.err != nil {
			a.recordBrokerResult(fmt.Errorf("kafka reader fetch: %w", message.err))
		} else {
			a.recordBrokerResult(nil)
		}
		if err := a.Send(a.coordinator, MessageBatchFetched{
			BatchID: message.operationID,
			Records: brokers.CloneMessages(message.records),
			Err:     message.err,
		}); err != nil {
			return fmt.Errorf("kafka reader: report fetch: %w", err)
		}
		return a.maybeCommitOrDrain()
	case MessageKafkaReaderCommitResult:
		_, active := a.meta.activeIO[message.source]
		if from != a.PID() || !active || a.operation.kind != kafkaReaderCommitting || message.operationID != a.operation.id {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_commit_result")
			return nil
		}
		operation := a.operation
		if !slices.Equal(message.sources, operation.commitRefs) {
			return fmt.Errorf("%w: commit completion changed candidate", errKafkaReaderProtocol)
		}
		a.operation = kafkaReaderOperation{}
		notice := MessageCommitResult{
			OperationID: message.operationID,
			Sources:     append([]RecordRef(nil), operation.commitRefs...),
			Err:         message.err,
		}
		if message.err != nil {
			a.recordBrokerResult(fmt.Errorf("kafka reader commit: %w", message.err))
			if err := a.Send(a.coordinator, notice); err != nil {
				return fmt.Errorf("kafka reader: report failed commit: %w", err)
			}
			a.stopMeta()
			return nil
		}
		a.removeCommitted(operation)
		a.recordBrokerResult(nil)
		if err := a.Send(a.coordinator, notice); err != nil {
			return fmt.Errorf("kafka reader: report commit: %w", err)
		}
		return a.maybeCommitOrDrain()
	case MessageKafkaReaderRetryProgress:
		_, active := a.meta.activeIO[message.source]
		if from != a.PID() || !active || message.operationID != a.operation.id || message.kind != a.operation.kind || message.err == nil || (message.kind != kafkaReaderFetching && message.kind != kafkaReaderCommitting) {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_retry_progress")
			return nil
		}
		a.recordBrokerResult(fmt.Errorf("kafka reader operation retry: %w", message.err))
	case MessageKafkaReaderIOStopped:
		if from != a.Parent() {
			a.labels.Count(a, metricKafkaReaderRejections, "foreign_io_stopped")
			return nil
		}
		if _, ok := a.meta.activeIO[message.Alias]; !ok {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_io_stopped")
			return nil
		}
		if message.Alias == a.meta.alias {
			a.metaLost(message.Alias, fmt.Errorf("kafka reader meta stopped without down notification"))
		}
		delete(a.meta.activeIO, message.Alias)
		if len(a.meta.activeIO) != 0 || a.meta.alias != (gen.Alias{}) {
			return nil
		}
		if a.lifecycle == KafkaReaderDraining && a.operation.kind == kafkaReaderIdle && len(a.ledger.entriesByRef) == 0 {
			a.meta.replacementPending = false
			return a.maybeDrained()
		}
		if len(a.ledger.entriesByRef) != 0 || a.operation.kind == kafkaReaderCommitting {
			reason := a.meta.status.err
			if reason == nil {
				reason = errors.New("reader state requires replay")
			}
			return fmt.Errorf("kafka reader session recovery required: %w", reason)
		}
		// lastFetchedOffsets retains the committed floor and detects broker regression after recovery.
		return a.scheduleMetaRestart()
	case gen.MessageDownAlias:
		if message.Alias != a.meta.alias {
			a.labels.Count(a, metricKafkaReaderRejections, "stale_meta_down")
			return nil
		}
		reason := message.Reason
		if reason == nil {
			reason = errors.New("unknown termination reason")
		}
		a.metaLost(message.Alias, fmt.Errorf("kafka reader meta stopped: %w", reason))
		return nil
	case gen.MessageDownPID:
		if a.coordinator == (gen.PID{}) || message.PID != a.coordinator {
			return nil
		}
		reason := message.Reason
		if reason == nil {
			reason = errors.New("unknown termination reason")
		}
		return fmt.Errorf("kafka reader coordinator stopped: %w", reason)
	}
	return nil
}

// HandleCall serves only the local status request.
func (a *kafkaReaderActor) HandleCall(from gen.PID, _ gen.Ref, request any) (any, error) {
	if _, ok := request.(KafkaReaderStatusRequest); ok {
		if from.Node != a.PID().Node {
			a.labels.Count(a, metricKafkaReaderRejections, "foreign_status")
			return gen.ErrNotAllowed, nil
		}
		return KafkaReaderStatusResponse{
			Session: a.PID(), Status: a.status(), Records: len(a.ledger.entriesByRef), Bytes: a.ledger.pendingBytes,
		}, nil
	}
	return fmt.Errorf("kafka reader: unsupported call %T", request), nil
}

// Terminate asks the meta to cancel its active deadline-bound I/O and publishes final state.
func (a *kafkaReaderActor) Terminate(reason error) {
	a.err = runtime.FirstError(reason, a.err)
	a.lifecycle = KafkaReaderStopped
	a.meta.status.lifecycle = KafkaReaderMetaStopped
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

// dispatchFetch sends the active fetch operation to the running reader meta.
func (a *kafkaReaderActor) dispatchFetch() error {
	if a.operation.kind != kafkaReaderFetching || a.meta.alias == (gen.Alias{}) || a.meta.status.lifecycle != KafkaReaderMetaRunning {
		return nil
	}
	a.operation.id = a.Node().MakeRef()
	if err := a.Send(a.meta.alias, MessageKafkaReaderFetch{operationID: a.operation.id, records: a.operation.fetchLimit}); err != nil {
		a.meta.status.err = fmt.Errorf("kafka reader: dispatch fetch: %w", err)
		a.stopMeta()
		return nil
	}
	return nil
}

// acceptFetched validates and appends fetched records to the ledger.
func (a *kafkaReaderActor) acceptFetched(records []brokers.Message) error {
	newBytes := brokers.TotalBytes(records)
	if newBytes < 0 || newBytes > a.opts.MaxPendingBytes-a.ledger.pendingBytes {
		return fmt.Errorf("%w: fetched bytes exceed pending-byte budget %d", errKafkaReaderCapacity, a.opts.MaxPendingBytes)
	}
	proposed := make(map[kafkaReaderPartition]int64)
	seen := make(map[RecordRef]struct{}, len(records))
	for _, record := range records {
		ref := RecordRef{Topic: record.Topic, Partition: record.Partition, Offset: record.Offset}
		partition := kafkaReaderPartition{topic: ref.Topic, partition: ref.Partition}
		if ref.Topic == "" || ref.Partition < 0 || ref.Offset < 0 {
			return fmt.Errorf("%w: invalid source %+v", errKafkaReaderProtocol, ref)
		}
		if _, duplicate := seen[ref]; duplicate {
			return fmt.Errorf("%w: duplicate source %+v", errKafkaReaderProtocol, ref)
		}
		seen[ref] = struct{}{}
		last, exists := proposed[partition]
		if !exists {
			last, exists = a.ledger.lastFetchedOffsets[partition]
		}
		if exists && ref.Offset <= last {
			return fmt.Errorf("%w: non-increasing source %+v after offset %d", errKafkaReaderProtocol, ref, last)
		}
		proposed[partition] = ref.Offset
	}

	knownPartitions := make(map[kafkaReaderPartition]struct{}, len(a.ledger.entriesByPartition))
	for partition := range a.ledger.entriesByPartition {
		knownPartitions[partition] = struct{}{}
	}
	for _, record := range records {
		ref := RecordRef{Topic: record.Topic, Partition: record.Partition, Offset: record.Offset}
		partition := kafkaReaderPartition{topic: ref.Topic, partition: ref.Partition}
		if _, known := knownPartitions[partition]; !known {
			a.ledger.partitionOrder = append(a.ledger.partitionOrder, partition)
			knownPartitions[partition] = struct{}{}
		}
		entry := &kafkaReaderLedgerEntry{ref: ref, byteCount: brokers.TotalBytes([]brokers.Message{record})}
		a.ledger.entriesByPartition[partition] = append(a.ledger.entriesByPartition[partition], entry)
		a.ledger.entriesByRef[ref] = entry
	}
	for partition, offset := range proposed {
		a.ledger.lastFetchedOffsets[partition] = offset
	}
	a.ledger.pendingBytes += newBytes
	return nil
}

// maybeCommitOrDrain commits resolved prefixes or completes draining.
func (a *kafkaReaderActor) maybeCommitOrDrain() error {
	if a.operation.kind != kafkaReaderIdle {
		return nil
	}
	refs, counts, positions := a.resolvedCommitCandidate()
	if len(refs) != 0 {
		if a.meta.alias == (gen.Alias{}) || a.meta.status.lifecycle != KafkaReaderMetaRunning {
			return nil
		}
		id := a.Node().MakeRef()
		a.operation = kafkaReaderOperation{
			kind: kafkaReaderCommitting, id: id, commitRefs: refs, commitCounts: counts,
		}
		job := MessageKafkaReaderCommit{
			operationID: id, sources: append([]RecordRef(nil), refs...), positions: positions,
		}
		if err := a.Send(a.meta.alias, job); err != nil {
			a.meta.status.err = fmt.Errorf("kafka reader: dispatch commit: %w", err)
			a.stopMeta()
			return nil
		}
		return nil
	}
	return a.maybeDrained()
}

// resolvedCommitCandidate returns each partition's resolved ledger prefix.
func (a *kafkaReaderActor) resolvedCommitCandidate() ([]RecordRef, map[kafkaReaderPartition]int, []brokers.Message) {
	counts := make(map[kafkaReaderPartition]int)
	var refs []RecordRef
	var positions []brokers.Message
	for _, partition := range a.ledger.partitionOrder {
		entries := a.ledger.entriesByPartition[partition]
		prefix := 0
		for prefix < len(entries) && entries[prefix].resolved {
			refs = append(refs, entries[prefix].ref)
			prefix++
		}
		if prefix == 0 {
			continue
		}
		counts[partition] = prefix
		last := entries[prefix-1].ref
		positions = append(positions, brokers.Message{Topic: last.Topic, Partition: last.Partition, Offset: last.Offset})
	}
	return refs, counts, positions
}

// recordBrokerResult updates broker availability and restart state.
func (a *kafkaReaderActor) recordBrokerResult(err error) {
	if err != nil {
		a.meta.consecutiveFailures++
		a.meta.status.availability = runtime.AvailabilityDegraded
		a.meta.status.err = err
		if a.meta.consecutiveFailures >= kafkaReaderUnavailableThreshold {
			a.meta.status.availability = runtime.AvailabilityUnavailable
		}
		return
	}
	a.meta.consecutiveFailures = 0
	if a.meta.status.lifecycle == KafkaReaderMetaRunning {
		a.meta.status.availability = runtime.AvailabilityReady
		a.meta.status.err = nil
	}
	a.meta.restart.CancelScheduled(true)
}

// removeCommitted removes the committed ledger prefixes.
func (a *kafkaReaderActor) removeCommitted(operation kafkaReaderOperation) {
	for _, partition := range a.ledger.partitionOrder {
		prefix := operation.commitCounts[partition]
		if prefix == 0 {
			continue
		}
		entries := a.ledger.entriesByPartition[partition]
		for _, entry := range entries[:prefix] {
			delete(a.ledger.entriesByRef, entry.ref)
			a.ledger.pendingBytes -= entry.byteCount
		}
		a.ledger.entriesByPartition[partition] = entries[prefix:]
	}
}

// maybeDrained transitions an idle empty reader to drained.
func (a *kafkaReaderActor) maybeDrained() error {
	if a.lifecycle != KafkaReaderDraining || a.operation.kind != kafkaReaderIdle || len(a.ledger.entriesByRef) != 0 {
		return nil
	}
	a.lifecycle = KafkaReaderDrained
	a.meta.replacementPending = false
	a.meta.restart.CancelScheduled(false)
	return nil
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// startMeta starts a replacement reader meta when restart conditions allow it.
func (a *kafkaReaderActor) startMeta() error {
	if !a.canRestartMeta() || !a.meta.replacementPending || a.meta.alias != (gen.Alias{}) || len(a.meta.activeIO) != 0 {
		return nil
	}
	a.meta.status = kafkaReaderMetaStatus{
		lifecycle:    KafkaReaderMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		err:          a.meta.status.err,
	}
	alias, err := a.SpawnMeta(&kafkaReaderMeta{
		newReader: a.newReader, barrier: a.barrier, fetchTimeout: a.opts.FetchTimeout,
		commitTimeout: a.opts.CommitTimeout, retryMin: a.opts.RetryMin, retryMax: a.opts.RetryMax,
		supervisor: a.Parent(), labels: a.labels,
	}, gen.MetaOptions{MailboxSize: 1})
	if err != nil {
		a.meta.status.err = fmt.Errorf("kafka reader: spawn meta: %w", err)
		return a.scheduleMetaRestart()
	}
	a.meta.activeIO[alias] = struct{}{}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.meta.status.err = fmt.Errorf("kafka reader: monitor meta: %w", err)
		return nil
	}
	a.meta.alias = alias
	a.meta.replacementPending = false
	return nil
}

// scheduleMetaRestart schedules a fenced reader meta replacement.
func (a *kafkaReaderActor) scheduleMetaRestart() error {
	if !a.canRestartMeta() || !a.meta.replacementPending || a.meta.alias != (gen.Alias{}) || len(a.meta.activeIO) != 0 || a.meta.restart.Pending {
		return nil
	}
	delay := a.meta.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("kafka reader meta restart: %w", runtime.ErrBackoffStopped)
	}
	a.meta.restart.Token++
	token := a.meta.restart.Token
	cancel, err := a.SendWithPriorityAfter(a.PID(), MessageKafkaReaderRestart{token: token}, gen.MessagePriorityHigh, delay)
	if err != nil {
		return fmt.Errorf("kafka reader: schedule meta restart: %w", err)
	}
	a.meta.restart.Pending = true
	a.meta.restart.Cancel = cancel
	a.meta.status.lifecycle = KafkaReaderMetaRestarting
	a.meta.status.availability = runtime.AvailabilityUnavailable
	return nil
}

// canRestartMeta reports whether the actor may replace its reader meta.
func (a *kafkaReaderActor) canRestartMeta() bool {
	if a.lifecycle == KafkaReaderStarting || a.lifecycle == KafkaReaderRunning {
		return true
	}
	return a.lifecycle == KafkaReaderDraining && a.operation.kind == kafkaReaderFetching
}

// metaLost records a lost reader meta and awaits its I/O stop proof.
func (a *kafkaReaderActor) metaLost(alias gen.Alias, reason error) {
	if alias == a.meta.alias {
		a.meta.alias = gen.Alias{}
		_ = a.DemonitorAlias(alias)
	}
	a.meta.replacementPending = true
	a.meta.status.lifecycle = KafkaReaderMetaRestarting
	a.meta.status.availability = runtime.AvailabilityUnavailable
	a.meta.status.err = reason
}

// stopMeta requests reader meta termination.
func (a *kafkaReaderActor) stopMeta() {
	if a.meta.alias == (gen.Alias{}) {
		return
	}
	alias := a.meta.alias
	a.metaLost(alias, a.meta.status.err)
	_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// status returns the current reader lifecycle and availability status.
func (a *kafkaReaderActor) status() KafkaReaderStatus {
	status := KafkaReaderStatus{Lifecycle: a.lifecycle, Availability: runtime.AvailabilityUnavailable}
	if a.lifecycle == KafkaReaderRunning {
		status.Availability = a.meta.status.availability
		if status.Availability == runtime.AvailabilityReady && a.err != nil {
			status.Availability = runtime.AvailabilityDegraded
		}
	}
	// The actor's own failure wins over its meta's.
	status.Err = runtime.FirstError(a.err, a.meta.status.err)
	if a.lifecycle != KafkaReaderStopped && a.lifecycle != KafkaReaderDrained {
		status.Fetching = a.operation.kind == kafkaReaderFetching
		status.Committing = a.operation.kind == kafkaReaderCommitting
	}
	return status
}

// publishGauges publishes reader status and ledger gauges.
func (a *kafkaReaderActor) publishGauges() {
	kafkaReaderActorGauges{
		availability:  a.status().Availability,
		ledgerRecords: len(a.ledger.entriesByRef),
		ledgerBytes:   a.ledger.pendingBytes,
		active:        a.operation.kind != kafkaReaderIdle,
		draining:      a.lifecycle == KafkaReaderDraining || a.lifecycle == KafkaReaderDrained,
	}.publish(a.labels, a)
}

// reconcileStatus refreshes gauges on every reconciliation and propagates status only on change.
func (a *kafkaReaderActor) reconcileStatus() {
	a.publishGauges()
	next := a.status()
	if sameKafkaReaderStatus(a.lastStatus, next) {
		return
	}
	wasDrained := a.lastStatus.Lifecycle == KafkaReaderDrained
	a.lastStatusEpoch = runtime.NextStatusEpoch(a.lastStatusEpoch)
	a.lastStatus = next
	a.propagateStatus(next)
	if next.Lifecycle == KafkaReaderDrained && !wasDrained {
		a.propagateDrainedStatus(next)
	}
}

// propagateStatus sends the supplied snapshot to the parent without reconciling state or gauges.
func (a *kafkaReaderActor) propagateStatus(next KafkaReaderStatus) {
	_ = a.SendWithPriority(a.Parent(), MessageKafkaReaderStatusChanged{StatusEpoch: a.lastStatusEpoch, Status: next}, gen.MessagePriorityHigh)
}

// propagateDrainedStatus keeps the coordinator's terminal notification behind normal-priority results.
func (a *kafkaReaderActor) propagateDrainedStatus(next KafkaReaderStatus) {
	if a.coordinator != (gen.PID{}) && a.coordinator != a.Parent() {
		_ = a.Send(a.coordinator, MessageKafkaReaderStatusChanged{StatusEpoch: a.lastStatusEpoch, Status: next})
	}
}

// HandleInspect exposes lifecycle, fencing identities, and immutable ledger counters.
func (a *kafkaReaderActor) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	queue := 0
	if a.operation.kind != kafkaReaderIdle {
		queue = 1
	}
	return map[string]string{
		"kafka_reader:lifecycle":                 string(status.Lifecycle),
		"kafka_reader:availability":              string(status.Availability),
		"kafka_reader:err":                       runtime.ErrorText(status.Err),
		"kafka_reader:session":                   a.PID().String(),
		"kafka_reader:meta:alias":                a.meta.alias.String(),
		"kafka_reader:meta:lifecycle":            string(a.meta.status.lifecycle),
		"kafka_reader:meta:availability":         string(a.meta.status.availability),
		"kafka_reader:meta:err":                  runtime.ErrorText(a.meta.status.err),
		"kafka_reader:fetching":                  fmt.Sprintf("%t", status.Fetching),
		"kafka_reader:committing":                fmt.Sprintf("%t", status.Committing),
		"kafka_reader:meta:active_io":            fmt.Sprintf("%d", len(a.meta.activeIO)),
		"kafka_reader:meta:replacement":          fmt.Sprintf("%t", a.meta.replacementPending),
		"kafka_reader:meta:consecutive_failures": fmt.Sprintf("%d", a.meta.consecutiveFailures),
		"kafka_reader:meta:restart_pending":      fmt.Sprintf("%t", a.meta.restart.Pending),
		"kafka_reader:coordinator":               a.coordinator.String(),
		"kafka_reader:ledger_records":            fmt.Sprintf("%d", len(a.ledger.entriesByRef)),
		"kafka_reader:ledger_bytes":              fmt.Sprintf("%d", a.ledger.pendingBytes),
		"kafka_reader:queue":                     fmt.Sprintf("%d", queue),
	}
}

// sameKafkaReaderStatus compares the status fields that trigger publication.
func sameKafkaReaderStatus(left, right KafkaReaderStatus) bool {
	return left.Lifecycle == right.Lifecycle &&
		left.Availability == right.Availability &&
		runtime.ErrorText(left.Err) == runtime.ErrorText(right.Err) &&
		left.Fetching == right.Fetching &&
		left.Committing == right.Committing
}

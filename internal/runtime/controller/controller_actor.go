// Package controller provides an Ergo-owned control runtime for one plugin catalog.
package controller

import (
	"fmt"
	"sort"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

type ActorLifecycle string

const (
	ActorStarting ActorLifecycle = "starting"
	ActorRunning  ActorLifecycle = "running"
	ActorDraining ActorLifecycle = "draining"
	ActorDrained  ActorLifecycle = "drained"
	ActorStopped  ActorLifecycle = "stopped"
)

// actorStatus is the controller actor's immutable status report to its supervisor.
type actorStatus struct {
	Lifecycle    ActorLifecycle
	Availability runtime.Availability
	Generation   int64
}

type scannerMetaState struct {
	alias      gen.Alias
	restart    *runtime.ScheduledBackoff
	status     artifactScannerMetaStatus
	entries    []snapshot.EffectiveEntry
	presentIDs []string
}

type writerMetaState struct {
	alias               gen.Alias
	consecutiveFailures int
	restart             *runtime.ScheduledBackoff
	status              snapshotWriterMetaStatus
	activeIO            map[gen.Alias]struct{}
	replacementPending  bool
}

type reconcilePlan struct {
	recordUpserts []backends.ControllerRecord
	next          snapshot.Snapshot
	entryUpserts  []snapshot.EffectiveEntry
	tombstones    []string
}

type actor[T plugin.Artifact] struct {
	act.Actor
	opts                ActorOptions[T]
	database            backends.Database
	barrier             *writerIOBarrier
	lifecycle           ActorLifecycle
	scanner             scannerMetaState
	writer              writerMetaState
	bootstrapped        bool
	generation          int64
	committed           *snapshot.Snapshot
	records             map[string]backends.ControllerRecord
	pending             *reconcilePlan
	fullRewriteRequired bool
	subscribers         map[string]gen.PID
	executors           map[string]ExecutorStatus
}

// newActor constructs the controller actor with normalized options.
func newActor[T plugin.Artifact](opts ActorOptions[T], database backends.Database, barrier *writerIOBarrier) gen.ProcessBehavior {
	return &actor[T]{opts: actorOptionsWithDefaults("", opts), database: database, barrier: barrier}
}

// --- messages ---

type MessageActorActivate struct{}
type MessageArtifactScannerMetaRestart struct{ token uint64 }
type MessageSnapshotWriterMetaRestart struct{ token uint64 }
type MessageExecutorDriftCheck struct{}
type MessageWriteSnapshot struct {
	records    []backends.ControllerRecord
	next       snapshot.Snapshot
	changed    bool
	upserts    []snapshot.EffectiveEntry
	tombstones []string
}

// StatusRequest asks for a snapshot of every tracked executor, for the controller's own /status endpoint; it never crosses the cluster, so it stays local instead of living in the wire vocabulary.
type StatusRequest struct{}

// StatusResponse answers StatusRequest with only what the actor knows - the caller already knows which namespace it queried.
type StatusResponse struct {
	Generation int64
	Executors  []ExecutorStatus
}

// ExecutorStatus is one executor's last-known convergence state, tracked per namespace controller actor; local for the same reason as StatusRequest.
type ExecutorStatus struct {
	ExecutorID          string
	LastSeen            time.Time
	CommittedGeneration int64
	ReadyGeneration     int64
	Availability        string
	LastError           string
	DriftSince          time.Time // zero if not currently drifting
}

// Apply folds one convergence report into status for its executor.
func (status ExecutorStatus) Apply(report snapshot.MessageExecutorReport) ExecutorStatus {
	status.ExecutorID = report.ExecutorID
	status.LastSeen = time.Now()
	if report.Heartbeat != nil {
		status.CommittedGeneration = report.Heartbeat.CommittedGeneration
		status.ReadyGeneration = report.Heartbeat.ReadyGeneration
		status.Availability = report.Heartbeat.Availability
	}
	if report.Applied != nil {
		status.ReadyGeneration = report.Applied.Generation
	}
	if report.LastError != "" {
		status.LastError = report.LastError
	}
	return status
}

// --- messages ---

const writeUnavailableThreshold = writeRetryAttemptBudget

const (
	// executorDriftCheckInterval is how often the actor re-evaluates every registered executor.
	executorDriftCheckInterval = 30 * time.Second
	// executorStaleThreshold is how long an executor may go without a report before drift evaluation excludes it.
	executorStaleThreshold = 2 * time.Minute
	// executorDriftGrace is how long an executor may lag before it's flagged as drifting.
	executorDriftGrace = 2 * time.Minute
)

// Init validates dependencies and initializes controller state.
func (a *actor[T]) Init(...any) error {
	if a.opts.Directory == "" || a.opts.Loader == nil || a.database == nil || a.barrier == nil {
		return fmt.Errorf("controller actor: directory, loader, database, and barrier are required")
	}
	a.lifecycle = ActorStarting
	a.records = make(map[string]backends.ControllerRecord)
	a.subscribers = make(map[string]gen.PID)
	a.executors = make(map[string]ExecutorStatus)
	a.scanner = scannerMetaState{
		restart: runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		status: artifactScannerMetaStatus{
			Lifecycle:    ArtifactScannerMetaStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
	a.writer = writerMetaState{
		restart: runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		status: snapshotWriterMetaStatus{
			Lifecycle:    SnapshotWriterMetaStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
		activeIO: make(map[gen.Alias]struct{}),
	}
	return nil
}

// HandleMessage advances controller state from lifecycle and worker messages.
func (a *actor[T]) HandleMessage(from gen.PID, message any) error {
	defer a.reportStatus()
	switch message.(type) {
	case MessageActorActivate:
		if from != a.Parent() || a.lifecycle != ActorStarting {
			return nil
		}
	case plugin.MessageDrain:
		if from != a.Parent() || a.lifecycle == ActorStarting {
			return nil
		}
		return a.beginDrain()
	case plugin.MessageStop:
		if from != a.Parent() {
			return nil
		}
		return gen.TerminateReasonNormal
	}
	if a.lifecycle == ActorDraining || a.lifecycle == ActorDrained {
		switch message.(type) {
		case gen.MessageDownAlias, gen.MessageDownPID, MessageSnapshotWriterIOStopped, snapshot.UnsubscribeRequest:
		default:
			return nil
		}
	}
	switch m := message.(type) {
	case MessageActorActivate:
		a.lifecycle = ActorRunning
		if err := a.startScanner(); err != nil {
			return err
		}
		if err := a.startWriter(); err != nil {
			return err
		}
		if _, err := a.SendAfter(a.PID(), MessageExecutorDriftCheck{}, executorDriftCheckInterval); err != nil {
			return fmt.Errorf("schedule executor drift check: %w", err)
		}
		a.Log().Info("controller activated: name=%s scanner_alias=%s writer_alias=%s", a.opts.Name, a.scanner.alias, a.writer.alias)
		return nil
	case snapshot.MessageExecutorReport:
		a.executors[m.ExecutorID] = a.executors[m.ExecutorID].Apply(m)
		return nil
	case MessageExecutorDriftCheck:
		a.checkExecutorDrift()
		if a.lifecycle == ActorRunning {
			if _, err := a.SendAfter(a.PID(), MessageExecutorDriftCheck{}, executorDriftCheckInterval); err != nil {
				return fmt.Errorf("reschedule executor drift check: %w", err)
			}
		}
		return nil
	case MessageArtifactScanResult:
		if from != a.PID() || m.source != a.scanner.alias {
			return nil
		}
		a.scanner.status.Lifecycle = ArtifactScannerMetaRunning
		if !m.complete {
			a.scanner.status.Complete = false
			a.scanner.status.Availability = runtime.AvailabilityUnavailable
			a.scanner.status.LastError = m.err
			return nil
		}
		a.scanner.restart.CancelScheduled(true)
		a.scanner.status.Complete = true
		if m.err != nil {
			a.scanner.status.Availability = runtime.AvailabilityDegraded
			a.scanner.status.LastError = m.err
		} else {
			a.scanner.status.Availability = runtime.AvailabilityReady
			a.scanner.status.LastError = nil
		}
		a.scanner.entries = snapshot.CloneEntries(m.entries)
		a.scanner.presentIDs = append([]string(nil), m.presentIDs...)
		a.Log().Debug("artifact scan accepted: name=%s entries=%d present_ids=%d nonfatal_error=%t", a.opts.Name, len(m.entries), len(m.presentIDs), m.err != nil)
		return a.reconcile()
	case MessageSnapshotLoadResult:
		if from != a.PID() || m.source != a.writer.alias {
			return nil
		}
		if m.err != nil {
			a.writer.status.LastError = m.err
			a.stopWriter(gen.TerminateReasonShutdown)
			a.writer.replacementPending = true
			return a.scheduleWriterRestart()
		}
		if !a.bootstrapped {
			a.bootstrapped = true
			a.generation = m.generation
			if m.snapshot != nil && m.snapshot.Generation > a.generation {
				a.generation = m.snapshot.Generation
			}
			a.fullRewriteRequired = m.snapshot == nil || m.snapshot.Generation != m.generation
			a.committed = m.snapshot.Clone()
			for _, record := range m.records {
				a.records[record.Id] = record
			}
		}
		a.writer.status.Lifecycle = SnapshotWriterMetaRunning
		a.writer.status.Loaded = true
		switch {
		case a.pending != nil, a.writer.status.LastError != nil, a.writer.consecutiveFailures >= writeUnavailableThreshold:
			a.writer.status.Availability = runtime.AvailabilityUnavailable
		case a.writer.consecutiveFailures > 0:
			a.writer.status.Availability = runtime.AvailabilityDegraded
		default:
			a.writer.status.Availability = runtime.AvailabilityReady
		}
		if a.pending != nil {
			return a.sendPending()
		}
		return a.reconcile()
	case MessageSnapshotWriteResult:
		if from != a.PID() || m.source != a.writer.alias || a.pending == nil || !a.writer.status.Writing {
			return nil
		}
		if m.err != nil {
			a.recordWriteFailure(m.err)
			return nil
		}
		a.writer.status.Writing = false
		changed := a.pending.next.Generation != a.generation
		priorCommitted := a.committed
		a.committed = a.pending.next.Clone()
		a.generation = a.pending.next.Generation
		a.fullRewriteRequired = false
		if changed {
			a.Log().Info("snapshot committed: name=%s generation=%d upserts=%d tombstones=%d", a.opts.Name, a.pending.next.Generation, len(a.pending.entryUpserts), len(a.pending.tombstones))
			a.notifySubscribers(snapshot.SnapshotUpdate{
				Snapshot:   a.committed.Clone(),
				Changes:    ClassifyChanges(a.opts.Loader, priorCommitted, a.pending.entryUpserts),
				Tombstones: append([]string(nil), a.pending.tombstones...),
			})
		}
		for _, record := range a.pending.recordUpserts {
			a.records[record.Id] = record
		}
		a.pending = nil
		a.writer.consecutiveFailures = 0
		a.writer.status.Availability = runtime.AvailabilityReady
		a.writer.status.LastError = nil
		a.writer.restart.CancelScheduled(true)
		return a.reconcile()
	case snapshot.UnsubscribeRequest:
		if pid, ok := a.subscribers[m.ExecutorID]; ok {
			delete(a.subscribers, m.ExecutorID)
			_ = a.DemonitorPID(pid)
		}
		return nil
	case gen.MessageDownPID:
		for id, pid := range a.subscribers {
			if pid == m.PID {
				delete(a.subscribers, id)
				break
			}
		}
		return nil
	case MessageArtifactScannerMetaRestart:
		if !a.scanner.restart.Pending || m.token != a.scanner.restart.Token || a.scanner.alias != (gen.Alias{}) {
			return nil
		}
		a.scanner.restart.Pending = false
		a.scanner.restart.Cancel = nil
		return a.startScanner()
	case MessageSnapshotWriterMetaRestart:
		if !a.writer.restart.Pending || m.token != a.writer.restart.Token || a.writer.alias != (gen.Alias{}) || len(a.writer.activeIO) != 0 {
			return nil
		}
		a.writer.restart.Pending = false
		a.writer.restart.Cancel = nil
		a.writer.replacementPending = false
		return a.startWriter()
	case MessageSnapshotWriterIOStopped:
		if from != a.Parent() {
			return nil
		}
		if _, ok := a.writer.activeIO[m.Alias]; !ok {
			return nil
		}
		delete(a.writer.activeIO, m.Alias)
		if a.lifecycle == ActorDraining || a.lifecycle == ActorDrained {
			return a.maybeDrained()
		}
		return a.scheduleWriterRestart()
	case gen.MessageDownAlias:
		switch m.Alias {
		case a.scanner.alias:
			if a.lifecycle == ActorRunning {
				a.Log().Error("artifact scanner stopped unexpectedly: name=%s alias=%s reason=%v", a.opts.Name, m.Alias, m.Reason)
			}
			a.scanner.alias = gen.Alias{}
			if a.lifecycle == ActorDraining || a.lifecycle == ActorDrained {
				a.scanner.status.Lifecycle, a.scanner.status.Availability = ArtifactScannerMetaStopped, runtime.AvailabilityUnavailable
				return nil
			}
			a.scanner.status.Lifecycle = ArtifactScannerMetaRestarting
			a.scanner.status.Availability = runtime.AvailabilityUnavailable
			a.scanner.status.Complete = false
			a.scanner.status.LastError = m.Reason
			return a.scheduleScannerRestart()
		case a.writer.alias:
			if a.lifecycle == ActorRunning {
				a.Log().Error("snapshot writer stopped unexpectedly: name=%s alias=%s reason=%v", a.opts.Name, m.Alias, m.Reason)
			}
			a.writer.alias = gen.Alias{}
			a.writer.status.Lifecycle = SnapshotWriterMetaRestarting
			a.writer.status.Availability = runtime.AvailabilityUnavailable
			a.writer.status.Loaded = false
			a.writer.status.Writing = false
			a.writer.status.LastError = m.Reason
			if a.lifecycle == ActorDraining || a.lifecycle == ActorDrained {
				return a.maybeDrained()
			}
			a.writer.replacementPending = true
			return a.scheduleWriterRestart()
		}
	}
	return nil
}

// Terminate stops workers and reports the final controller state.
func (a *actor[T]) Terminate(error) {
	defer a.reportStatus()
	a.lifecycle = ActorStopped
	a.scanner.restart.CancelScheduled(false)
	a.writer.restart.CancelScheduled(false)
	a.stopScanner(gen.TerminateReasonShutdown)
	a.stopWriter(gen.TerminateReasonShutdown)
	a.scanner.status.Lifecycle = ArtifactScannerMetaStopped
	a.scanner.status.Availability = runtime.AvailabilityUnavailable
	a.writer.status.Lifecycle = SnapshotWriterMetaStopped
	a.writer.status.Availability = runtime.AvailabilityUnavailable
	a.writer.status.Writing = false
}

// startScanner starts a new artifact scanner instance.
func (a *actor[T]) startScanner() error {
	if a.scanner.alias != (gen.Alias{}) {
		return nil
	}
	a.scanner.status = artifactScannerMetaStatus{Lifecycle: ArtifactScannerMetaStarting, Availability: runtime.AvailabilityUnavailable}
	alias, err := a.SpawnMeta(&artifactScannerMeta[T]{directory: a.opts.Directory, loader: a.opts.Loader}, gen.MetaOptions{})
	if err != nil {
		a.scanner.status.LastError = fmt.Errorf("spawn artifact scanner meta: %w", err)
		a.Log().Error("artifact scanner meta spawn failed: name=%s error=%v", a.opts.Name, a.scanner.status.LastError)
		return a.scheduleScannerRestart()
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.scanner.status.LastError = fmt.Errorf("monitor artifact scanner meta: %w", err)
		a.Log().Error("artifact scanner meta monitor failed: name=%s error=%v", a.opts.Name, a.scanner.status.LastError)
		return a.scheduleScannerRestart()
	}
	a.scanner.alias = alias
	return nil
}

// startWriter starts a new snapshot writer when I/O is unfenced.
func (a *actor[T]) startWriter() error {
	if a.writer.alias != (gen.Alias{}) || len(a.writer.activeIO) != 0 || a.lifecycle != ActorRunning {
		return nil
	}
	a.writer.status = snapshotWriterMetaStatus{
		Lifecycle:    SnapshotWriterMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
		LastError:    a.writer.status.LastError,
	}
	alias, err := a.SpawnMeta(&snapshotWriterMeta{
		database:   a.database,
		barrier:    a.barrier,
		supervisor: a.Parent(),
		retryMin:   a.opts.RetryMin,
		retryMax:   a.opts.RetryMax,
	}, gen.MetaOptions{})
	if err != nil {
		a.writer.status.LastError = fmt.Errorf("spawn snapshot writer meta: %w", err)
		a.Log().Error("snapshot writer meta spawn failed: name=%s error=%v", a.opts.Name, a.writer.status.LastError)
		a.writer.replacementPending = true
		return a.scheduleWriterRestart()
	}
	a.writer.activeIO[alias] = struct{}{}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.writer.status.LastError = fmt.Errorf("monitor snapshot writer meta: %w", err)
		a.Log().Error("snapshot writer meta monitor failed: name=%s error=%v", a.opts.Name, a.writer.status.LastError)
		a.writer.replacementPending = true
		return a.scheduleWriterRestart()
	}
	a.writer.alias = alias
	a.writer.replacementPending = false
	return nil
}

// stopScanner terminates the active artifact scanner.
func (a *actor[T]) stopScanner(reason error) {
	if a.scanner.alias != (gen.Alias{}) {
		alias := a.scanner.alias
		a.scanner.alias = gen.Alias{}
		_ = a.DemonitorAlias(alias)
		_ = a.SendExitMeta(alias, reason)
	}
}

// stopWriter terminates the active snapshot writer.
func (a *actor[T]) stopWriter(reason error) {
	if a.writer.alias != (gen.Alias{}) {
		alias := a.writer.alias
		a.writer.alias = gen.Alias{}
		_ = a.DemonitorAlias(alias)
		_ = a.SendExitMeta(alias, reason)
	}
}

// scheduleScannerRestart arranges the next scanner restart attempt.
func (a *actor[T]) scheduleScannerRestart() error {
	if a.lifecycle != ActorRunning || a.scanner.restart.Pending {
		return nil
	}
	delay := a.scanner.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact scanner restart: %w", runtime.ErrBackoffStopped)
	}
	a.scanner.restart.Token++
	token := a.scanner.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageArtifactScannerMetaRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact scanner restart: %w", err)
	}
	a.scanner.restart.Pending = true
	a.scanner.restart.Cancel = cancel
	a.scanner.status.Lifecycle = ArtifactScannerMetaRestarting
	a.Log().Debug("artifact scanner restart scheduled: name=%s delay=%s token=%d", a.opts.Name, delay, token)
	return nil
}

// scheduleWriterRestart arranges a writer restart after its I/O stops.
func (a *actor[T]) scheduleWriterRestart() error {
	if a.lifecycle != ActorRunning || !a.writer.replacementPending || a.writer.alias != (gen.Alias{}) || len(a.writer.activeIO) != 0 || a.writer.restart.Pending {
		return nil
	}
	delay := a.writer.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot writer restart: %w", runtime.ErrBackoffStopped)
	}
	a.writer.restart.Token++
	token := a.writer.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageSnapshotWriterMetaRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule snapshot writer restart: %w", err)
	}
	a.writer.restart.Pending = true
	a.writer.restart.Cancel = cancel
	a.writer.status.Lifecycle = SnapshotWriterMetaRestarting
	a.Log().Debug("snapshot writer restart scheduled: name=%s delay=%s token=%d", a.opts.Name, delay, token)
	return nil
}

// reconcile builds and queues a plan when both inputs are ready.
func (a *actor[T]) reconcile() error {
	if !a.bootstrapped || !a.scanner.status.Complete || a.pending != nil {
		return nil
	}
	plan := makePlan(a.committed, a.generation, a.records, a.scanner.entries, a.scanner.presentIDs, a.fullRewriteRequired, time.Now())
	a.pending = &plan
	return a.sendPending()
}

// sendPending queues the current plan with the active writer.
func (a *actor[T]) sendPending() error {
	if a.pending == nil || a.writer.status.Writing || a.writer.alias == (gen.Alias{}) || !a.writer.status.Loaded || a.lifecycle != ActorRunning {
		return nil
	}
	a.writer.status.Writing = true
	message := MessageWriteSnapshot{
		records:    append([]backends.ControllerRecord(nil), a.pending.recordUpserts...),
		next:       *a.pending.next.Clone(),
		changed:    a.pending.next.Generation != a.generation,
		upserts:    snapshot.CloneEntries(a.pending.entryUpserts),
		tombstones: append([]string(nil), a.pending.tombstones...),
	}
	for i := range message.records {
		message.records[i] = message.records[i].Clone()
	}
	if err := a.Send(a.writer.alias, message); err != nil {
		a.Log().Error("pending write dispatch failed: name=%s generation=%d error=%v", a.opts.Name, message.next.Generation, err)
		a.writer.status.Writing = false
		a.recordWriteFailure(fmt.Errorf("%w: queue write: %w", runtime.ErrSnapshotWrite, err))
		a.writer.status.Availability = runtime.AvailabilityUnavailable
		a.writer.status.Loaded = false
		a.writer.replacementPending = true
		a.stopWriter(gen.TerminateReasonShutdown)
		return a.scheduleWriterRestart()
	}
	a.Log().Debug("pending write dispatched: name=%s generation=%d changed=%t upserts=%d tombstones=%d", a.opts.Name, message.next.Generation, message.changed, len(message.upserts), len(message.tombstones))
	return nil
}

// recordWriteFailure updates writer health after a failed write.
func (a *actor[T]) recordWriteFailure(err error) {
	a.writer.consecutiveFailures++
	a.writer.status.Availability = runtime.AvailabilityDegraded
	a.writer.status.LastError = err
	if a.writer.consecutiveFailures >= writeUnavailableThreshold {
		a.writer.status.Availability = runtime.AvailabilityUnavailable
	}
}

// beginDrain stops workers and waits for accepted writer I/O.
func (a *actor[T]) beginDrain() error {
	if a.lifecycle == ActorDraining || a.lifecycle == ActorDrained || a.lifecycle == ActorStopped {
		return nil
	}
	a.lifecycle = ActorDraining
	a.Log().Info("controller draining: name=%s writer_active_io=%d", a.opts.Name, len(a.writer.activeIO))
	a.scanner.restart.CancelScheduled(false)
	a.writer.restart.CancelScheduled(false)
	a.stopScanner(gen.TerminateReasonShutdown)
	a.stopWriter(gen.TerminateReasonShutdown)
	return a.maybeDrained()
}

// maybeDrained marks the controller drained after writer I/O completes.
func (a *actor[T]) maybeDrained() error {
	if len(a.writer.activeIO) != 0 {
		return nil
	}
	if a.lifecycle != ActorDrained {
		a.lifecycle = ActorDrained
		a.Log().Info("controller drained: name=%s", a.opts.Name)
	}
	return nil
}

// reportStatus sends the current status to the supervisor.
func (a *actor[T]) reportStatus() {
	availability := runtime.AvailabilityUnavailable
	if a.lifecycle == ActorRunning {
		availability = runtime.AvailabilityDegraded
		if a.scanner.status.Complete && a.scanner.status.Availability == runtime.AvailabilityReady && a.writer.status.Loaded && a.writer.status.Availability == runtime.AvailabilityReady {
			availability = runtime.AvailabilityReady
		}
	}
	_ = a.Send(a.Parent(), MessageActorStatusChanged{status: actorStatus{Lifecycle: a.lifecycle, Availability: availability, Generation: a.generation}})
}

// makePlan derives persistence updates and keyed snapshot changes.
func makePlan(prior *snapshot.Snapshot, generation int64, records map[string]backends.ControllerRecord, entries []snapshot.EffectiveEntry, presentIDs []string, fullRewriteRequired bool, now time.Time) reconcilePlan {
	present := make(map[string]struct{}, len(presentIDs))
	for _, id := range presentIDs {
		present[id] = struct{}{}
	}
	priorRecords := make(map[string]*backends.ControllerRecord, len(records))
	for id, record := range records {
		copy := record
		priorRecords[id] = &copy
	}
	upsertRecords := make([]backends.ControllerRecord, 0, len(records)+len(presentIDs))
	for _, id := range presentIDs {
		record, ok := records[id]
		if !ok {
			record = backends.ControllerRecord{Id: id, FirstSeenAt: now}
		}
		record.LastSeenAt = now
		record.Status = backends.StatusActive
		upsertRecords = append(upsertRecords, record)
		copy := record
		priorRecords[id] = &copy
	}
	previousIDs := make([]string, 0, len(records))
	for id := range records {
		previousIDs = append(previousIDs, id)
	}
	sort.Strings(previousIDs)
	for _, id := range previousIDs {
		if _, ok := present[id]; ok || records[id].Status != backends.StatusActive {
			continue
		}
		record := records[id]
		record.Status = backends.StatusAbsent
		upsertRecords = append(upsertRecords, record)
		copy := record
		priorRecords[id] = &copy
	}
	nextEntries := snapshot.CloneEntries(entries)
	valid := make(map[string]struct{}, len(nextEntries))
	for _, entry := range nextEntries {
		valid[entry.Id] = struct{}{}
	}
	if prior != nil {
		for _, entry := range prior.Entries {
			if _, ok := valid[entry.Id]; !ok {
				if _, present := present[entry.Id]; present {
					nextEntries = append(nextEntries, entry)
				}
			}
		}
	}
	sort.Slice(nextEntries, func(i, j int) bool { return nextEntries[i].Id < nextEntries[j].Id })
	changed := fullRewriteRequired || SnapshotChanged(nextEntries, prior)
	nextGeneration := generation
	if changed {
		nextGeneration++
	}
	next := snapshot.Snapshot{Generation: nextGeneration, Entries: nextEntries}
	diffPrior := prior
	if fullRewriteRequired {
		diffPrior = nil
	}
	upserts, tombstones := DiffEntries(diffPrior, nextEntries, priorRecords)
	sort.Slice(upserts, func(i, j int) bool { return upserts[i].Id < upserts[j].Id })
	sort.Strings(tombstones)
	return reconcilePlan{recordUpserts: upsertRecords, next: next, entryUpserts: upserts, tombstones: tombstones}
}

// HandleCall answers a subscribing executor's request; everything else is rejected.
func (a *actor[T]) HandleCall(from gen.PID, _ gen.Ref, request any) (any, error) {
	switch m := request.(type) {
	case snapshot.SubscribeRequest:
		a.subscribers[m.ExecutorID] = from
		_ = a.MonitorPID(from)
		return snapshot.SubscribeResponse{
			Current:       a.committed.Clone(),
			Changes:       ClassifyChanges(a.opts.Loader, nil, a.committedUpserts()),
			ControllerPID: a.PID(),
		}, nil
	case StatusRequest:
		executors := make([]ExecutorStatus, 0, len(a.executors))
		for _, status := range a.executors {
			executors = append(executors, status)
		}
		sort.Slice(executors, func(i, j int) bool { return executors[i].ExecutorID < executors[j].ExecutorID })
		return StatusResponse{Generation: a.generation, Executors: executors}, nil
	}
	return fmt.Errorf("controller actor: unsupported call %T", request), nil
}

// committedUpserts returns every committed entry as an upsert, for a fresh subscriber's initial burst (nil prior in ClassifyChanges makes each one ChangeAdded).
func (a *actor[T]) committedUpserts() []snapshot.EffectiveEntry {
	if a.committed == nil {
		return nil
	}
	return snapshot.CloneEntries(a.committed.Entries)
}

// notifySubscribers pushes a new commit to every subscriber PID; a failed SendImportant is logged but doesn't unregister it - MonitorPID's MessageDownPID is the actual removal path.
func (a *actor[T]) notifySubscribers(update snapshot.SnapshotUpdate) {
	for id, pid := range a.subscribers {
		if err := a.SendImportant(pid, update); err != nil {
			a.Log().Error("snapshot push failed: name=%s executor_id=%s pid=%s error=%v", a.opts.Name, id, pid, err)
		}
	}
}

// checkExecutorDrift updates DriftSince for every tracked executor against the committed generation, skipping a stale one (no report inside executorStaleThreshold) since silence is a liveness problem, not drift evidence.
func (a *actor[T]) checkExecutorDrift() {
	now := time.Now()
	for id, status := range a.executors {
		stale := now.Sub(status.LastSeen) > executorStaleThreshold
		behind := status.ReadyGeneration < a.generation
		switch {
		case stale || !behind:
			if !status.DriftSince.IsZero() {
				status.DriftSince = time.Time{}
				a.executors[id] = status
			}
		case status.DriftSince.IsZero():
			status.DriftSince = now
			a.executors[id] = status
		default:
			driftDuration := now.Sub(status.DriftSince)
			// Log once around the grace threshold crossing, not on every tick it stays drifting.
			if driftDuration > executorDriftGrace && driftDuration-executorDriftCheckInterval <= executorDriftGrace {
				a.Log().Error("executor drift detected: name=%s executor_id=%s ready_generation=%d committed_generation=%d drift=%s", a.opts.Name, id, status.ReadyGeneration, a.generation, driftDuration)
			}
		}
	}
}

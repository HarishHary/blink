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
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/snapshot"
)

type ControllerActorLifecycle string

const (
	ControllerActorStarting ControllerActorLifecycle = "starting"
	ControllerActorRunning  ControllerActorLifecycle = "running"
	ControllerActorDraining ControllerActorLifecycle = "draining"
	ControllerActorDrained  ControllerActorLifecycle = "drained"
	ControllerActorStopped  ControllerActorLifecycle = "stopped"
)

// ControllerActorStatus is the controller actor's immutable status report to its supervisor.
type ControllerActorStatus struct {
	Lifecycle    ControllerActorLifecycle
	Availability runtime.Availability
	Generation   int64
	Pending      bool
	Publishing   bool
	Scanner      ArtifactScannerStatus
	Publisher    SnapshotPublisherStatus
}

type scannerMetaState struct {
	alias      gen.Alias
	restart    *runtime.ScheduledBackoff
	status     ArtifactScannerStatus
	entries    []snapshot.EffectiveEntry
	presentIDs []string
}

type publisherMetaState struct {
	alias               gen.Alias
	consecutiveFailures int
	restart             *runtime.ScheduledBackoff
	status              SnapshotPublisherStatus
	activeIO            map[gen.Alias]struct{}
	replacementPending  bool
}

type reconcilePlan struct {
	recordUpserts []backends.ControllerRecord
	next          snapshot.Snapshot
	entryUpserts  []snapshot.EffectiveEntry
	tombstones    []string
}

type controllerActor[T plugin.Syncable] struct {
	act.Actor
	opts                  ControllerActorOptions[T]
	database              backends.Database
	writer                brokers.Writer
	barrier               *publisherIOBarrier
	lifecycle             ControllerActorLifecycle
	scanner               scannerMetaState
	publisher             publisherMetaState
	bootstrapped          bool
	generation            int64
	committed             *snapshot.Snapshot
	records               map[string]backends.ControllerRecord
	pending               *reconcilePlan
	fullRepublishRequired bool
}

// newActor constructs the controller actor with normalized options.
func newActor[T plugin.Syncable](opts ControllerActorOptions[T], database backends.Database, writer brokers.Writer, barrier *publisherIOBarrier) gen.ProcessBehavior {
	return &controllerActor[T]{opts: controllerActorOptionsWithDefaults("", opts), database: database, writer: writer, barrier: barrier}
}

// --- messages ---

type MessageControllerActivate struct{}
type MessageArtifactScannerRestart struct{ token uint64 }
type MessageSnapshotPublisherRestart struct{ token uint64 }
type MessagePublishSnapshot struct {
	records    []backends.ControllerRecord
	next       snapshot.Snapshot
	changed    bool
	upserts    []snapshot.EffectiveEntry
	tombstones []string
}

// --- messages ---

const publishUnavailableThreshold = publishRetryAttemptBudget

// Init validates dependencies and initializes controller state.
func (a *controllerActor[T]) Init(...any) error {
	if a.opts.Directory == "" || a.opts.Loader == nil || a.database == nil || a.writer == nil || a.barrier == nil {
		return fmt.Errorf("controller actor: directory, loader, database, writer, and barrier are required")
	}
	a.lifecycle = ControllerActorStarting
	a.records = make(map[string]backends.ControllerRecord)
	a.scanner = scannerMetaState{
		restart: runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		status: ArtifactScannerStatus{
			Lifecycle:    ArtifactScannerStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
	a.publisher = publisherMetaState{
		restart: runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		status: SnapshotPublisherStatus{
			Lifecycle:    SnapshotPublisherStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
		activeIO: make(map[gen.Alias]struct{}),
	}
	return nil
}

// HandleMessage advances controller state from lifecycle and worker messages.
func (a *controllerActor[T]) HandleMessage(from gen.PID, message any) error {
	defer a.reportStatus()
	switch message.(type) {
	case MessageControllerActivate:
		if from != a.Parent() || a.lifecycle != ControllerActorStarting {
			return nil
		}
	case plugin.MessageDrain:
		if from != a.Parent() || a.lifecycle == ControllerActorStarting {
			return nil
		}
		return a.beginDrain()
	case plugin.MessageStop:
		if from != a.Parent() {
			return nil
		}
		return gen.TerminateReasonNormal
	}
	if a.lifecycle == ControllerActorDraining || a.lifecycle == ControllerActorDrained {
		switch message.(type) {
		case gen.MessageDownAlias, MessageSnapshotPublisherIOStopped:
		default:
			return nil
		}
	}
	switch m := message.(type) {
	case MessageControllerActivate:
		a.lifecycle = ControllerActorRunning
		if err := a.startScanner(); err != nil {
			return err
		}
		if err := a.startPublisher(); err != nil {
			return err
		}
		return nil
	case MessageArtifactScanResult:
		if from != a.PID() || m.source != a.scanner.alias {
			return nil
		}
		a.scanner.status.Lifecycle = ArtifactScannerRunning
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
		a.scanner.entries = cloneEntries(m.entries)
		a.scanner.presentIDs = append([]string(nil), m.presentIDs...)
		return a.reconcile()
	case MessageSnapshotLoadResult:
		if from != a.PID() || m.source != a.publisher.alias {
			return nil
		}
		if m.err != nil {
			a.publisher.status.LastError = m.err
			a.stopPublisher(gen.TerminateReasonShutdown)
			a.publisher.replacementPending = true
			return a.schedulePublisherRestart()
		}
		if !a.bootstrapped {
			a.bootstrapped = true
			a.generation = m.generation
			if m.snapshot != nil && m.snapshot.Generation > a.generation {
				a.generation = m.snapshot.Generation
			}
			a.fullRepublishRequired = m.snapshot == nil || m.snapshot.Generation != m.generation
			a.committed = m.snapshot.Clone()
			for _, record := range m.records {
				a.records[record.Id] = record
			}
		}
		a.publisher.status.Lifecycle = SnapshotPublisherRunning
		a.publisher.status.Loaded = true
		switch {
		case a.pending != nil, a.publisher.status.LastError != nil, a.publisher.consecutiveFailures >= publishUnavailableThreshold:
			a.publisher.status.Availability = runtime.AvailabilityUnavailable
		case a.publisher.consecutiveFailures > 0:
			a.publisher.status.Availability = runtime.AvailabilityDegraded
		default:
			a.publisher.status.Availability = runtime.AvailabilityReady
		}
		if a.pending != nil {
			return a.sendPending()
		}
		return a.reconcile()
	case MessageSnapshotPublishResult:
		if from != a.PID() || m.source != a.publisher.alias || a.pending == nil || !a.publisher.status.Publishing {
			return nil
		}
		if m.err != nil {
			a.recordPublishFailure(m.err)
			return nil
		}
		a.publisher.status.Publishing = false
		a.committed = a.pending.next.Clone()
		a.generation = a.pending.next.Generation
		a.fullRepublishRequired = false
		for _, record := range a.pending.recordUpserts {
			a.records[record.Id] = record
		}
		a.pending = nil
		a.publisher.consecutiveFailures = 0
		a.publisher.status.Availability = runtime.AvailabilityReady
		a.publisher.status.LastError = nil
		a.publisher.restart.CancelScheduled(true)
		return a.reconcile()
	case MessageArtifactScannerRestart:
		if !a.scanner.restart.Pending || m.token != a.scanner.restart.Token || a.scanner.alias != (gen.Alias{}) {
			return nil
		}
		a.scanner.restart.Pending = false
		a.scanner.restart.Cancel = nil
		return a.startScanner()
	case MessageSnapshotPublisherRestart:
		if !a.publisher.restart.Pending || m.token != a.publisher.restart.Token || a.publisher.alias != (gen.Alias{}) || len(a.publisher.activeIO) != 0 {
			return nil
		}
		a.publisher.restart.Pending = false
		a.publisher.restart.Cancel = nil
		a.publisher.replacementPending = false
		return a.startPublisher()
	case MessageSnapshotPublisherIOStopped:
		if from != a.Parent() {
			return nil
		}
		if _, ok := a.publisher.activeIO[m.Alias]; !ok {
			return nil
		}
		delete(a.publisher.activeIO, m.Alias)
		if a.lifecycle == ControllerActorDraining || a.lifecycle == ControllerActorDrained {
			return a.maybeDrained()
		}
		return a.schedulePublisherRestart()
	case gen.MessageDownAlias:
		switch m.Alias {
		case a.scanner.alias:
			a.scanner.alias = gen.Alias{}
			if a.lifecycle == ControllerActorDraining || a.lifecycle == ControllerActorDrained {
				a.scanner.status.Lifecycle, a.scanner.status.Availability = ArtifactScannerStopped, runtime.AvailabilityUnavailable
				return nil
			}
			a.scanner.status.Lifecycle = ArtifactScannerRestarting
			a.scanner.status.Availability = runtime.AvailabilityUnavailable
			a.scanner.status.Complete = false
			a.scanner.status.LastError = m.Reason
			return a.scheduleScannerRestart()
		case a.publisher.alias:
			a.publisher.alias = gen.Alias{}
			a.publisher.status.Lifecycle = SnapshotPublisherRestarting
			a.publisher.status.Availability = runtime.AvailabilityUnavailable
			a.publisher.status.Loaded = false
			a.publisher.status.Publishing = false
			a.publisher.status.LastError = m.Reason
			if a.lifecycle == ControllerActorDraining || a.lifecycle == ControllerActorDrained {
				return a.maybeDrained()
			}
			a.publisher.replacementPending = true
			return a.schedulePublisherRestart()
		}
	}
	return nil
}

// Terminate stops workers and reports the final controller state.
func (a *controllerActor[T]) Terminate(error) {
	a.lifecycle = ControllerActorStopped
	a.scanner.restart.CancelScheduled(false)
	a.publisher.restart.CancelScheduled(false)
	a.stopScanner(gen.TerminateReasonShutdown)
	a.stopPublisher(gen.TerminateReasonShutdown)
	a.scanner.status.Lifecycle = ArtifactScannerStopped
	a.scanner.status.Availability = runtime.AvailabilityUnavailable
	a.publisher.status.Lifecycle = SnapshotPublisherStopped
	a.publisher.status.Availability = runtime.AvailabilityUnavailable
	a.publisher.status.Publishing = false
	a.reportStatus()
}

// startScanner starts a new artifact scanner instance.
func (a *controllerActor[T]) startScanner() error {
	if a.scanner.alias != (gen.Alias{}) {
		return nil
	}
	a.scanner.status = ArtifactScannerStatus{Lifecycle: ArtifactScannerStarting, Availability: runtime.AvailabilityUnavailable}
	alias, err := a.SpawnMeta(&artifactScannerMeta[T]{directory: a.opts.Directory, loader: a.opts.Loader}, gen.MetaOptions{})
	if err != nil {
		a.scanner.status.LastError = fmt.Errorf("spawn artifact scanner meta: %w", err)
		return a.scheduleScannerRestart()
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.scanner.status.LastError = fmt.Errorf("monitor artifact scanner meta: %w", err)
		return a.scheduleScannerRestart()
	}
	a.scanner.alias = alias
	return nil
}

// startPublisher starts a new snapshot publisher when I/O is unfenced.
func (a *controllerActor[T]) startPublisher() error {
	if a.publisher.alias != (gen.Alias{}) || len(a.publisher.activeIO) != 0 || a.lifecycle != ControllerActorRunning {
		return nil
	}
	a.publisher.status = SnapshotPublisherStatus{
		Lifecycle:    SnapshotPublisherStarting,
		Availability: runtime.AvailabilityUnavailable,
		LastError:    a.publisher.status.LastError,
	}
	alias, err := a.SpawnMeta(&snapshotPublisherMeta{
		database:   a.database,
		writer:     a.writer,
		barrier:    a.barrier,
		supervisor: a.Parent(),
		retryMin:   a.opts.RetryMin,
		retryMax:   a.opts.RetryMax,
	}, gen.MetaOptions{})
	if err != nil {
		a.publisher.status.LastError = fmt.Errorf("spawn snapshot publisher meta: %w", err)
		a.publisher.replacementPending = true
		return a.schedulePublisherRestart()
	}
	a.publisher.activeIO[alias] = struct{}{}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.publisher.status.LastError = fmt.Errorf("monitor snapshot publisher meta: %w", err)
		a.publisher.replacementPending = true
		return a.schedulePublisherRestart()
	}
	a.publisher.alias = alias
	a.publisher.replacementPending = false
	return nil
}

// stopScanner terminates the active artifact scanner.
func (a *controllerActor[T]) stopScanner(reason error) {
	if a.scanner.alias != (gen.Alias{}) {
		alias := a.scanner.alias
		a.scanner.alias = gen.Alias{}
		_ = a.DemonitorAlias(alias)
		_ = a.SendExitMeta(alias, reason)
	}
}

// stopPublisher terminates the active snapshot publisher.
func (a *controllerActor[T]) stopPublisher(reason error) {
	if a.publisher.alias != (gen.Alias{}) {
		alias := a.publisher.alias
		a.publisher.alias = gen.Alias{}
		_ = a.DemonitorAlias(alias)
		_ = a.SendExitMeta(alias, reason)
	}
}

// scheduleScannerRestart arranges the next scanner restart attempt.
func (a *controllerActor[T]) scheduleScannerRestart() error {
	if a.lifecycle != ControllerActorRunning || a.scanner.restart.Pending {
		return nil
	}
	delay := a.scanner.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact scanner restart: %w", runtime.ErrBackoffStopped)
	}
	a.scanner.restart.Token++
	token := a.scanner.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageArtifactScannerRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact scanner restart: %w", err)
	}
	a.scanner.restart.Pending = true
	a.scanner.restart.Cancel = cancel
	a.scanner.status.Lifecycle = ArtifactScannerRestarting
	return nil
}

// schedulePublisherRestart arranges a publisher restart after its I/O stops.
func (a *controllerActor[T]) schedulePublisherRestart() error {
	if a.lifecycle != ControllerActorRunning || !a.publisher.replacementPending || a.publisher.alias != (gen.Alias{}) || len(a.publisher.activeIO) != 0 || a.publisher.restart.Pending {
		return nil
	}
	delay := a.publisher.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot publisher restart: %w", runtime.ErrBackoffStopped)
	}
	a.publisher.restart.Token++
	token := a.publisher.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageSnapshotPublisherRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule snapshot publisher restart: %w", err)
	}
	a.publisher.restart.Pending = true
	a.publisher.restart.Cancel = cancel
	a.publisher.status.Lifecycle = SnapshotPublisherRestarting
	return nil
}

// reconcile builds and queues a plan when both inputs are ready.
func (a *controllerActor[T]) reconcile() error {
	if !a.bootstrapped || !a.scanner.status.Complete || a.pending != nil {
		return nil
	}
	plan := makePlan(a.committed, a.generation, a.records, a.scanner.entries, a.scanner.presentIDs, a.fullRepublishRequired, time.Now())
	a.pending = &plan
	return a.sendPending()
}

// sendPending queues the current plan with the active publisher.
func (a *controllerActor[T]) sendPending() error {
	if a.pending == nil || a.publisher.status.Publishing || a.publisher.alias == (gen.Alias{}) || !a.publisher.status.Loaded || a.lifecycle != ControllerActorRunning {
		return nil
	}
	a.publisher.status.Publishing = true
	message := MessagePublishSnapshot{
		records:    append([]backends.ControllerRecord(nil), a.pending.recordUpserts...),
		next:       *a.pending.next.Clone(),
		changed:    a.pending.next.Generation != a.generation,
		upserts:    cloneEntries(a.pending.entryUpserts),
		tombstones: append([]string(nil), a.pending.tombstones...),
	}
	for i := range message.records {
		message.records[i] = message.records[i].Clone()
	}
	if err := a.Send(a.publisher.alias, message); err != nil {
		a.publisher.status.Publishing = false
		a.recordPublishFailure(fmt.Errorf("%w: queue publication: %w", runtime.ErrSnapshotPublish, err))
		a.publisher.status.Availability = runtime.AvailabilityUnavailable
		a.publisher.status.Loaded = false
		a.publisher.replacementPending = true
		a.stopPublisher(gen.TerminateReasonShutdown)
		return a.schedulePublisherRestart()
	}
	return nil
}

// recordPublishFailure updates publisher health after a failed publication.
func (a *controllerActor[T]) recordPublishFailure(err error) {
	a.publisher.consecutiveFailures++
	a.publisher.status.Availability = runtime.AvailabilityDegraded
	a.publisher.status.LastError = err
	if a.publisher.consecutiveFailures >= publishUnavailableThreshold {
		a.publisher.status.Availability = runtime.AvailabilityUnavailable
	}
}

// beginDrain stops workers and waits for accepted publisher I/O.
func (a *controllerActor[T]) beginDrain() error {
	if a.lifecycle == ControllerActorDraining || a.lifecycle == ControllerActorDrained || a.lifecycle == ControllerActorStopped {
		return nil
	}
	a.lifecycle = ControllerActorDraining
	a.scanner.restart.CancelScheduled(false)
	a.publisher.restart.CancelScheduled(false)
	a.stopScanner(gen.TerminateReasonShutdown)
	a.stopPublisher(gen.TerminateReasonShutdown)
	return a.maybeDrained()
}

// maybeDrained marks the controller drained after publisher I/O completes.
func (a *controllerActor[T]) maybeDrained() error {
	if len(a.publisher.activeIO) != 0 {
		return nil
	}
	a.lifecycle = ControllerActorDrained
	return nil
}

// reportStatus sends the current status to the supervisor.
func (a *controllerActor[T]) reportStatus() {
	_ = a.Send(a.Parent(), MessageControllerStatusChanged{status: a.currentStatus()})
}

// currentStatus derives aggregate controller health.
func (a *controllerActor[T]) currentStatus() ControllerActorStatus {
	availability := runtime.AvailabilityUnavailable
	if a.lifecycle == ControllerActorRunning {
		availability = runtime.AvailabilityDegraded
		if a.scanner.status.Complete && a.scanner.status.Availability == runtime.AvailabilityReady && a.publisher.status.Loaded && a.publisher.status.Availability == runtime.AvailabilityReady {
			availability = runtime.AvailabilityReady
		}
	}
	return ControllerActorStatus{Lifecycle: a.lifecycle, Availability: availability, Generation: a.generation, Pending: a.pending != nil, Publishing: a.publisher.status.Publishing, Scanner: a.scanner.status, Publisher: a.publisher.status}
}

// cloneEntries returns independent copies of catalog entries.
func cloneEntries(entries []snapshot.EffectiveEntry) []snapshot.EffectiveEntry {
	cloned := append([]snapshot.EffectiveEntry(nil), entries...)
	for i := range cloned {
		cloned[i] = cloned[i].Clone()
	}
	return cloned
}

// makePlan derives persistence updates and keyed snapshot changes.
func makePlan(prior *snapshot.Snapshot, generation int64, records map[string]backends.ControllerRecord, entries []snapshot.EffectiveEntry, presentIDs []string, fullRepublishRequired bool, now time.Time) reconcilePlan {
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
	nextEntries := cloneEntries(entries)
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
	changed := fullRepublishRequired || SnapshotChanged(nextEntries, prior)
	nextGeneration := generation
	if changed {
		nextGeneration++
	}
	next := snapshot.Snapshot{Generation: nextGeneration, Entries: nextEntries}
	diffPrior := prior
	if fullRepublishRequired {
		diffPrior = nil
	}
	upserts, tombstones := DiffEntries(diffPrior, nextEntries, priorRecords)
	sort.Slice(upserts, func(i, j int) bool { return upserts[i].Id < upserts[j].Id })
	sort.Strings(tombstones)
	return reconcilePlan{recordUpserts: upsertRecords, next: next, entryUpserts: upserts, tombstones: tombstones}
}

// HandleCall rejects unsupported controller calls.
func (a *controllerActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("controller actor: unsupported call %T", request), nil
}

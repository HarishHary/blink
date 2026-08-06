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
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// ControllerLifecycle describes the stable controller actor lifecycle.
type ControllerLifecycle string

const (
	ControllerStarting ControllerLifecycle = "starting"
	ControllerRunning  ControllerLifecycle = "running"
	ControllerStopped  ControllerLifecycle = "stopped"
)

// ControllerStatus groups the controller's durable publication state and both metas.
type ControllerStatus struct {
	Lifecycle    ControllerLifecycle
	Availability runtime.Availability
	Generation   int64
	Pending      bool
	Publishing   bool
	Scanner      ArtifactScannerStatus
	Publisher    SnapshotPublisherStatus
}

// Options configures one controller actor and its scanner/publisher metas.
type Options[T plugin.Syncable] struct {
	Directory  string
	Loader     config.Loader[T]
	Database   backends.Database
	Writer     brokers.Writer
	RestartMin time.Duration
	RestartMax time.Duration
	RetryMin   time.Duration
	RetryMax   time.Duration
}

// NewActor returns a controller actor behavior suitable for supervisor wiring.
func NewActor[T plugin.Syncable](opts Options[T]) gen.ProcessBehavior {
	return &controllerActor[T]{opts: defaultOptions(opts)}
}

type MessageControllerActivate struct{}
type MessageArtifactScannerRestart struct{ token uint64 }
type MessageSnapshotPublisherRestart struct{ token uint64 }
type MessageSnapshotPublishRetry struct{ token uint64 }

const publishUnavailableThreshold = 5

type scannerMetaState struct {
	alias        gen.Alias
	incarnation  uint64
	restartCount uint64
	restart      *runtime.ScheduledBackoff
	status       ArtifactScannerStatus
	entries      []snapshot.EffectiveEntry
	presentIDs   []string
}

type publisherMetaState struct {
	alias               gen.Alias
	incarnation         uint64
	restartCount        uint64
	consecutiveFailures int
	restart             *runtime.ScheduledBackoff
	retry               *runtime.ScheduledBackoff
	status              SnapshotPublisherStatus
}

type reconcilePlan struct {
	recordUpserts []backends.ControllerRecord
	next          snapshot.Snapshot
	entryUpserts  []snapshot.EffectiveEntry
	tombstones    []string
}

type controllerActor[T plugin.Syncable] struct {
	act.Actor

	opts Options[T]

	lifecycle ControllerLifecycle
	scanner   scannerMetaState
	publisher publisherMetaState

	bootstrapped bool
	generation   int64
	committed    *snapshot.Snapshot
	records      map[string]backends.ControllerRecord
	pending      *reconcilePlan
}

func (a *controllerActor[T]) Init(...any) error {
	if a.opts.Directory == "" || a.opts.Loader == nil || a.opts.Database == nil || a.opts.Writer == nil {
		return fmt.Errorf("controller actor: directory, loader, database, and writer are required")
	}
	a.lifecycle = ControllerStarting
	a.records = make(map[string]backends.ControllerRecord)
	a.scanner = scannerMetaState{
		restart: runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		status:  ArtifactScannerStatus{Lifecycle: ArtifactScannerStarting, Availability: runtime.AvailabilityUnavailable},
	}
	a.publisher = publisherMetaState{
		restart: runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax),
		retry:   runtime.NewScheduledBackoff(a.opts.RetryMin, a.opts.RetryMax),
		status:  SnapshotPublisherStatus{Lifecycle: SnapshotPublisherStarting, Availability: runtime.AvailabilityUnavailable},
	}
	return a.Send(a.PID(), MessageControllerActivate{})
}

func (a *controllerActor[T]) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case MessageControllerActivate:
		if a.lifecycle != ControllerStarting {
			return nil
		}
		a.lifecycle = ControllerRunning
		a.startScanner()
		a.startPublisher()
	case MessageArtifactScanResult:
		if a.scanner.alias == (gen.Alias{}) || m.incarnation != a.scanner.incarnation {
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
		a.scanner.status.RestartPending = false
		if m.err != nil {
			a.scanner.status.Availability = runtime.AvailabilityDegraded
			a.scanner.status.LastError = m.err
		} else {
			a.scanner.status.Availability = runtime.AvailabilityReady
			a.scanner.status.LastError = nil
		}
		a.scanner.entries = append([]snapshot.EffectiveEntry(nil), m.entries...)
		for i := range a.scanner.entries {
			a.scanner.entries[i] = a.scanner.entries[i].Clone()
		}
		a.scanner.presentIDs = append([]string(nil), m.presentIDs...)
		a.reconcile()
	case MessageSnapshotPublisherLoadResult:
		if a.publisher.alias == (gen.Alias{}) || m.incarnation != a.publisher.incarnation {
			return nil
		}
		if m.err != nil {
			a.publisher.status.LastError = m.err
			a.stopPublisher(gen.TerminateReasonShutdown)
			a.schedulePublisherRestart()
			return nil
		}
		if !a.bootstrapped {
			a.bootstrapped = true
			a.generation = m.generation
			if m.snapshot != nil && m.snapshot.Generation > a.generation {
				a.generation = m.snapshot.Generation
			}
			a.committed = m.snapshot.Clone()
			for _, record := range m.records {
				a.records[record.Id] = record
			}
		}
		a.publisher.restart.CancelScheduled(true)
		a.cancelPublishRetry(false)
		a.publisher.status.Lifecycle = SnapshotPublisherRunning
		if a.publisher.consecutiveFailures >= publishUnavailableThreshold {
			a.publisher.status.Availability = runtime.AvailabilityUnavailable
		} else {
			a.publisher.status.Availability = runtime.AvailabilityReady
		}
		a.publisher.status.Loaded = true
		a.publisher.status.RestartPending = false
		a.publisher.status.LastError = nil
		if a.pending != nil {
			a.sendPending()
		} else {
			a.reconcile()
		}
	case MessageSnapshotPublicationResult:
		if a.publisher.alias == (gen.Alias{}) || m.incarnation != a.publisher.incarnation || a.pending == nil || !a.publisher.status.Publishing {
			return nil
		}
		a.publisher.status.Publishing = false
		if m.err != nil {
			a.recordPublishFailure(m.err)
			a.schedulePublishRetry()
			return nil
		}
		a.committed = a.pending.next.Clone()
		a.generation = a.pending.next.Generation
		for _, record := range a.pending.recordUpserts {
			a.records[record.Id] = record
		}
		a.pending = nil
		a.publisher.consecutiveFailures = 0
		a.cancelPublishRetry(true)
		a.publisher.status.Availability = runtime.AvailabilityReady
		a.publisher.status.LastError = nil
		a.reconcile()
	case MessageSnapshotPublishRetry:
		if !a.publisher.retry.Pending || m.token != a.publisher.retry.Token {
			return nil
		}
		a.publisher.retry.Pending = false
		a.publisher.retry.Cancel = nil
		a.sendPending()
	case MessageArtifactScannerRestart:
		if !a.scanner.restart.Pending || m.token != a.scanner.restart.Token || a.scanner.alias != (gen.Alias{}) {
			return nil
		}
		a.scanner.restart.Pending = false
		a.scanner.restart.Cancel = nil
		a.startScanner()
	case MessageSnapshotPublisherRestart:
		if !a.publisher.restart.Pending || m.token != a.publisher.restart.Token || a.publisher.alias != (gen.Alias{}) {
			return nil
		}
		a.publisher.restart.Pending = false
		a.publisher.restart.Cancel = nil
		a.startPublisher()
	case gen.MessageDownAlias:
		switch m.Alias {
		case a.scanner.alias:
			a.scanner.alias = gen.Alias{}
			a.scanner.status.Lifecycle = ArtifactScannerRestarting
			a.scanner.status.Availability = runtime.AvailabilityUnavailable
			a.scanner.status.Complete = false
			a.scanner.status.LastError = m.Reason
			a.scheduleScannerRestart()
		case a.publisher.alias:
			a.publisher.alias = gen.Alias{}
			a.publisher.status.Lifecycle = SnapshotPublisherRestarting
			a.publisher.status.Availability = runtime.AvailabilityUnavailable
			a.publisher.status.Loaded = false
			a.publisher.status.Publishing = false
			a.publisher.status.LastError = m.Reason
			// A retry bound to the dead publisher must not suppress retransmission
			// after the replacement's successful load.
			a.cancelPublishRetry(false)
			a.schedulePublisherRestart()
		}
	}
	return nil
}

func (a *controllerActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("controller actor: unsupported call %T", request)
}

func (a *controllerActor[T]) Terminate(error) {
	a.lifecycle = ControllerStopped
	a.cancelPublishRetry(false)
	a.scanner.restart.CancelScheduled(false)
	a.publisher.restart.CancelScheduled(false)
	a.stopScanner(gen.TerminateReasonShutdown)
	a.stopPublisher(gen.TerminateReasonShutdown)
	a.scanner.status.Lifecycle = ArtifactScannerStopped
	a.scanner.status.Availability = runtime.AvailabilityUnavailable
	a.publisher.status.Lifecycle = SnapshotPublisherStopped
	a.publisher.status.Availability = runtime.AvailabilityUnavailable
	a.publisher.status.Publishing = false
}

func (a *controllerActor[T]) startScanner() {
	if a.scanner.alias != (gen.Alias{}) {
		return
	}
	if a.scanner.incarnation > 0 {
		a.scanner.restartCount++
	}
	a.scanner.incarnation++
	a.scanner.status = ArtifactScannerStatus{
		Lifecycle:    ArtifactScannerStarting,
		Availability: runtime.AvailabilityUnavailable,
		Incarnation:  a.scanner.incarnation,
		RestartCount: a.scanner.restartCount,
	}
	alias, err := a.SpawnMeta(&artifactScannerMeta[T]{
		directory:   a.opts.Directory,
		loader:      a.opts.Loader,
		incarnation: a.scanner.incarnation,
	}, gen.MetaOptions{})
	if err == nil {
		err = a.MonitorAlias(alias)
	}
	if err != nil {
		if alias != (gen.Alias{}) {
			_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		}
		a.scanner.status.LastError = fmt.Errorf("start artifact scanner meta: %w", err)
		a.scheduleScannerRestart()
		return
	}
	a.scanner.alias = alias
}

func (a *controllerActor[T]) startPublisher() {
	if a.publisher.alias != (gen.Alias{}) {
		return
	}
	if a.publisher.incarnation > 0 {
		a.publisher.restartCount++
	}
	a.publisher.incarnation++
	a.publisher.status = SnapshotPublisherStatus{
		Lifecycle:    SnapshotPublisherStarting,
		Availability: runtime.AvailabilityUnavailable,
		Incarnation:  a.publisher.incarnation,
		RestartCount: a.publisher.restartCount,
	}
	alias, err := a.SpawnMeta(&snapshotPublisherMeta{
		database:    a.opts.Database,
		writer:      a.opts.Writer,
		incarnation: a.publisher.incarnation,
	}, gen.MetaOptions{})
	if err == nil {
		err = a.MonitorAlias(alias)
	}
	if err != nil {
		if alias != (gen.Alias{}) {
			_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		}
		a.publisher.status.LastError = fmt.Errorf("start snapshot publisher meta: %w", err)
		a.schedulePublisherRestart()
		return
	}
	a.publisher.alias = alias
}

func (a *controllerActor[T]) stopScanner(reason error) {
	if a.scanner.alias == (gen.Alias{}) {
		return
	}
	alias := a.scanner.alias
	a.scanner.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

func (a *controllerActor[T]) stopPublisher(reason error) {
	if a.publisher.alias == (gen.Alias{}) {
		return
	}
	alias := a.publisher.alias
	a.publisher.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

func (a *controllerActor[T]) scheduleScannerRestart() {
	if a.scanner.restart.Pending {
		return
	}
	delay := a.scanner.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return
	}
	a.scanner.restart.Token++
	token := a.scanner.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageArtifactScannerRestart{token: token}, delay)
	if err == nil {
		a.scanner.restart.Pending = true
		a.scanner.restart.Cancel = cancel
		a.scanner.status.Lifecycle = ArtifactScannerRestarting
		a.scanner.status.RestartPending = true
	}
}

func (a *controllerActor[T]) schedulePublisherRestart() {
	if a.publisher.restart.Pending {
		return
	}
	delay := a.publisher.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return
	}
	a.publisher.restart.Token++
	token := a.publisher.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageSnapshotPublisherRestart{token: token}, delay)
	if err == nil {
		a.publisher.restart.Pending = true
		a.publisher.restart.Cancel = cancel
		a.publisher.status.Lifecycle = SnapshotPublisherRestarting
		a.publisher.status.RestartPending = true
	}
}

func (a *controllerActor[T]) schedulePublishRetry() {
	if a.publisher.retry.Pending {
		return
	}
	delay := a.publisher.retry.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return
	}
	a.publisher.retry.Token++
	token := a.publisher.retry.Token
	cancel, err := a.SendAfter(a.PID(), MessageSnapshotPublishRetry{token: token}, delay)
	if err == nil {
		a.publisher.retry.Pending = true
		a.publisher.retry.Cancel = cancel
	}
}

func (a *controllerActor[T]) cancelPublishRetry(reset bool) {
	a.publisher.retry.CancelScheduled(reset)
}

func (a *controllerActor[T]) reconcile() {
	if !a.bootstrapped || !a.scanner.status.Complete || a.pending != nil {
		return
	}
	plan := makePlan(a.committed, a.generation, a.records, a.scanner.entries, a.scanner.presentIDs, time.Now())
	a.pending = &plan
	a.sendPending()
}

func (a *controllerActor[T]) sendPending() {
	if a.pending == nil || a.publisher.status.Publishing || a.publisher.alias == (gen.Alias{}) || !a.publisher.status.Loaded {
		return
	}
	a.publisher.status.Publishing = true
	next := a.pending.next.Clone()
	records := append([]backends.ControllerRecord(nil), a.pending.recordUpserts...)
	for i := range records {
		records[i] = records[i].Clone()
	}
	upserts := append([]snapshot.EffectiveEntry(nil), a.pending.entryUpserts...)
	for i := range upserts {
		upserts[i] = upserts[i].Clone()
	}
	message := MessagePublishSnapshot{
		incarnation: a.publisher.incarnation,
		records:     records,
		next:        *next,
		changed:     a.pending.next.Generation != a.generation,
		upserts:     upserts,
		tombstones:  append([]string(nil), a.pending.tombstones...),
	}
	if err := a.Send(a.publisher.alias, message); err != nil {
		a.publisher.status.Publishing = false
		a.recordPublishFailure(fmt.Errorf("%w: queue publication: %w", runtime.ErrSnapshotPublish, err))
		a.schedulePublishRetry()
	}
}

func (a *controllerActor[T]) recordPublishFailure(err error) {
	a.publisher.consecutiveFailures++
	a.publisher.status.Availability = runtime.AvailabilityDegraded
	if a.publisher.consecutiveFailures >= publishUnavailableThreshold {
		a.publisher.status.Availability = runtime.AvailabilityUnavailable
	}
	a.publisher.status.LastError = err
}

func (a *controllerActor[T]) currentStatus() ControllerStatus {
	availability := runtime.AvailabilityUnavailable
	if a.lifecycle == ControllerRunning {
		availability = runtime.AvailabilityDegraded
		if a.scanner.status.Complete &&
			a.scanner.status.Availability == runtime.AvailabilityReady &&
			a.publisher.status.Loaded &&
			a.publisher.status.Availability == runtime.AvailabilityReady {
			availability = runtime.AvailabilityReady
		}
	}
	return ControllerStatus{
		Lifecycle:    a.lifecycle,
		Availability: availability,
		Generation:   a.generation,
		Pending:      a.pending != nil,
		Publishing:   a.publisher.status.Publishing,
		Scanner:      a.scanner.status,
		Publisher:    a.publisher.status,
	}
}

func makePlan(prior *snapshot.Snapshot, generation int64, records map[string]backends.ControllerRecord, entries []snapshot.EffectiveEntry, presentIDs []string, now time.Time) reconcilePlan {
	present := make(map[string]struct{}, len(presentIDs))
	for _, id := range presentIDs {
		present[id] = struct{}{}
	}
	priorRecords := make(map[string]*backends.ControllerRecord, len(records))
	for id, record := range records {
		recordCopy := record
		priorRecords[id] = &recordCopy
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
	}

	nextEntries := append([]snapshot.EffectiveEntry(nil), entries...)
	valid := make(map[string]struct{}, len(nextEntries))
	for _, entry := range nextEntries {
		valid[entry.Id] = struct{}{}
	}
	if prior != nil {
		for _, entry := range prior.Entries {
			if _, isValid := valid[entry.Id]; !isValid {
				if _, ok := present[entry.Id]; ok {
					nextEntries = append(nextEntries, entry)
				}
			}
		}
	}
	sort.Slice(nextEntries, func(i, j int) bool { return nextEntries[i].Id < nextEntries[j].Id })
	changed := SnapshotChanged(nextEntries, prior)
	nextGeneration := generation
	if changed {
		nextGeneration++
	}
	next := snapshot.Snapshot{Generation: nextGeneration, Entries: nextEntries}
	upserts, tombstones := DiffEntries(prior, nextEntries, priorRecords)
	sort.Slice(upserts, func(i, j int) bool { return upserts[i].Id < upserts[j].Id })
	sort.Strings(tombstones)
	return reconcilePlan{recordUpserts: upsertRecords, next: next, entryUpserts: upserts, tombstones: tombstones}
}

func defaultOptions[T plugin.Syncable](opts Options[T]) Options[T] {
	if opts.RestartMin <= 0 {
		opts.RestartMin = 100 * time.Millisecond
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = 5 * time.Second
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = opts.RestartMin
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RestartMax
	}
	return opts
}

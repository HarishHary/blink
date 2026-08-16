package snapshot

import (
	"fmt"
	"sort"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// ReaderActorLifecycle describes the stable snapshot-reader actor subtree.
type ReaderActorLifecycle string

const (
	ReaderActorStarting   ReaderActorLifecycle = "starting"
	ReaderActorRunning    ReaderActorLifecycle = "running"
	ReaderActorRestarting ReaderActorLifecycle = "restarting"
	ReaderActorStopped    ReaderActorLifecycle = "stopped"
)

// ReaderActorStatus is the public status value published by the supervisor.
type ReaderActorStatus struct {
	Lifecycle    ReaderActorLifecycle
	Availability runtime.Availability
	Generation   int64
}

type readerMetaState struct {
	alias   gen.Alias
	restart *runtime.ScheduledBackoff
	status  readerMetaStatus
}

type readerActor struct {
	act.Actor
	opts               ReaderActorOptions
	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref
	readerMeta         readerMetaState
	entries            map[string]snapshot.EffectiveEntry
	committed          *snapshot.Snapshot
}

// --- messages ---

type MessageReaderMetaRestart struct{ token uint64 }

type MessageReaderActorActivate struct {
	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref
}

// --- messages ---

// Init initializes reader state and its restart backoff.
func (a *readerActor) Init(...any) error {
	a.entries = make(map[string]snapshot.EffectiveEntry)
	a.readerMeta.status = readerMetaStatus{
		Lifecycle:    ReaderMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	a.readerMeta.restart = runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax)
	return nil
}

// HandleMessage processes reader records, lifecycle, and restart messages.
func (a *readerActor) HandleMessage(from gen.PID, message any) error {
	defer a.reportStatus()
	switch m := message.(type) {
	case MessageReaderActorActivate:
		if from != a.Parent() || a.snapshotEventToken != (gen.Ref{}) {
			return nil
		}
		a.snapshotEventName = m.snapshotEventName
		a.snapshotEventToken = m.snapshotEventToken
		return a.startReaderMeta()
	case MessageRecord:
		if m.source != a.readerMeta.alias || a.readerMeta.alias == (gen.Alias{}) {
			return nil
		}
		started := a.readerMeta.status.Lifecycle != ReaderMetaRunning
		if started {
			a.readerMeta.status.Lifecycle = ReaderMetaRunning
			a.readerMeta.status.Availability = runtime.AvailabilityDegraded
			a.readerMeta.status.CaughtUp = false
			a.readerMeta.status.LastError = nil
		}
		committed := a.apply(m.message) && a.readerMeta.status.CaughtUp
		if committed {
			a.readerMeta.status.Availability = runtime.AvailabilityReady
			a.readerMeta.status.LastError = nil
			a.publishSnapshot()
		}
	case MessageCaughtUp:
		if m.source != a.readerMeta.alias || a.readerMeta.alias == (gen.Alias{}) || a.readerMeta.status.CaughtUp {
			return nil
		}
		a.readerMeta.status.Lifecycle = ReaderMetaRunning
		a.readerMeta.status.Availability = runtime.AvailabilityReady
		a.readerMeta.status.CaughtUp = true
		a.readerMeta.restart.CancelScheduled(true)
		if a.committed == nil {
			a.readerMeta.status.Availability = runtime.AvailabilityUnavailable
			a.readerMeta.status.LastError = fmt.Errorf("%w: generation marker not found", runtime.ErrSnapshotRead)
			return nil
		}
		a.readerMeta.status.LastError = nil
		a.publishSnapshot()
	case MessageReaderMetaRestart:
		if !a.readerMeta.restart.Pending || a.readerMeta.restart.Token != m.token || a.readerMeta.alias != (gen.Alias{}) {
			return nil
		}
		a.readerMeta.restart.Pending = false
		a.readerMeta.restart.Cancel = nil
		return a.startReaderMeta()
	case gen.MessageDownAlias:
		if m.Alias != a.readerMeta.alias {
			return nil
		}
		a.readerMeta.alias = gen.Alias{}
		a.readerMeta.status.Lifecycle = ReaderMetaRestarting
		a.readerMeta.status.Availability = runtime.AvailabilityUnavailable
		a.readerMeta.status.CaughtUp = false
		a.readerMeta.status.LastError = m.Reason
		a.opts.Logger.ErrorF("snapshot reader actor: reader %s stopped: %v", m.Alias, m.Reason)
		return a.scheduleReaderMetaRestart()
	}
	return nil
}

// HandleCall rejects unsupported synchronous requests.
func (a *readerActor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot reader actor: unsupported call %T", request), nil
}

// Terminate stops the reader meta-process and marks it unavailable.
func (a *readerActor) Terminate(error) {
	defer a.reportStatus()
	a.readerMeta.restart.CancelScheduled(false)
	a.stopReaderMeta(gen.TerminateReasonShutdown)
	a.readerMeta.status.Lifecycle = ReaderMetaStopped
	a.readerMeta.status.Availability = runtime.AvailabilityUnavailable
	a.readerMeta.status.CaughtUp = false
}

// startReaderMeta starts compacted-topic reconstruction while retaining the prior published snapshot.
func (a *readerActor) startReaderMeta() error {
	a.stopReaderMeta(gen.TerminateReasonShutdown)
	a.readerMeta.status = readerMetaStatus{
		Lifecycle:    ReaderMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	a.entries = make(map[string]snapshot.EffectiveEntry)
	a.committed = nil

	reader := a.opts.ReaderFactory()
	if reader == nil {
		a.readerMeta.status.Lifecycle = ReaderMetaRestarting
		a.readerMeta.status.LastError = fmt.Errorf("start snapshot reader meta: reader factory returned nil")
		return a.scheduleReaderMetaRestart()
	}

	alias, err := a.SpawnMeta(&readerMeta{reader: reader}, gen.MetaOptions{})
	if err != nil {
		_ = reader.Close()
		a.readerMeta.status.Lifecycle = ReaderMetaRestarting
		a.readerMeta.status.LastError = fmt.Errorf("spawn snapshot reader meta: %w", err)
		return a.scheduleReaderMetaRestart()
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.readerMeta.status.Lifecycle = ReaderMetaRestarting
		a.readerMeta.status.LastError = fmt.Errorf("monitor snapshot reader meta: %w", err)
		return a.scheduleReaderMetaRestart()
	}
	a.readerMeta.alias = alias
	return nil
}

// stopReaderMeta stops the active reader meta-process.
func (a *readerActor) stopReaderMeta(reason error) {
	if a.readerMeta.alias == (gen.Alias{}) {
		return
	}
	alias := a.readerMeta.alias
	a.readerMeta.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

// scheduleReaderMetaRestart schedules a backoff-delayed reader restart.
func (a *readerActor) scheduleReaderMetaRestart() error {
	if a.readerMeta.restart.Pending {
		return nil
	}
	delay := a.readerMeta.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot reader meta restart: %w", runtime.ErrBackoffStopped)
	}
	a.readerMeta.restart.Token++
	token := a.readerMeta.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageReaderMetaRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule snapshot reader meta restart: %w", err)
	}
	a.readerMeta.restart.Pending = true
	a.readerMeta.restart.Cancel = cancel
	a.readerMeta.status.Lifecycle = ReaderMetaRestarting
	a.readerMeta.status.Availability = runtime.AvailabilityUnavailable
	return nil
}

// apply incorporates a compacted-topic record and reports a new committed generation.
func (a *readerActor) apply(message brokers.Message) bool {
	key := string(message.Key)
	if key == snapshot.GenerationMarkerKey {
		generation, err := snapshot.DecodeGeneration(message.Value)
		if err != nil {
			a.opts.Logger.ErrorF("snapshot reader actor: decode generation marker: %v", err)
			return false
		}
		if a.committed != nil && generation <= a.committed.Generation {
			if generation == a.committed.Generation {
				return false
			}
			a.opts.Logger.ErrorF(
				"snapshot reader actor: generation went backwards %d -> %d",
				a.committed.Generation,
				generation,
			)
			return false
		}
		a.committed = &snapshot.Snapshot{
			Generation: generation,
			Entries:    a.sortedEntries(),
		}
		return true
	}

	if len(message.Value) == 0 {
		delete(a.entries, key)
		return false
	}

	entry, err := snapshot.Unmarshal(message.Value)
	if err != nil {
		a.opts.Logger.ErrorF("snapshot reader actor: unmarshal entry %q: %v", key, err)
		return false
	}
	a.entries[entry.Id] = entry
	return false
}

// publishSnapshot sends the committed snapshot to subscribers.
func (a *readerActor) publishSnapshot() {
	if a.committed == nil {
		return
	}
	if a.snapshotEventToken != (gen.Ref{}) {
		_ = a.SendEvent(a.snapshotEventName, a.snapshotEventToken, a.committed.Clone())
	}
}

// sortedEntries returns cloned entries ordered by identifier.
func (a *readerActor) sortedEntries() []snapshot.EffectiveEntry {
	entries := make([]snapshot.EffectiveEntry, 0, len(a.entries))
	for _, entry := range a.entries {
		entries = append(entries, entry.Clone())
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Id < entries[j].Id })
	return entries
}

// reportStatus sends current reader status to the supervisor.
func (a *readerActor) reportStatus() {
	if a.snapshotEventToken == (gen.Ref{}) {
		return
	}

	availability := runtime.AvailabilityUnavailable
	switch a.readerMeta.status.Availability {
	case runtime.AvailabilityReady:
		availability = runtime.AvailabilityReady
	case runtime.AvailabilityDegraded:
		availability = runtime.AvailabilityDegraded
	}

	status := ReaderActorStatus{
		Lifecycle:    ReaderActorRunning,
		Availability: availability,
	}
	if a.committed != nil {
		status.Generation = a.committed.Generation
	}
	_ = a.Send(a.Parent(), MessageReaderStatusChanged{status: status})
}

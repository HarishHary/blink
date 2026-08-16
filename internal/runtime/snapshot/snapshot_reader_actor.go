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

// SnapshotReaderLifecycle describes the stable snapshot-reader actor subtree.
type SnapshotReaderLifecycle string

const (
	SnapshotReaderStarting   SnapshotReaderLifecycle = "starting"
	SnapshotReaderRunning    SnapshotReaderLifecycle = "running"
	SnapshotReaderRestarting SnapshotReaderLifecycle = "restarting"
	SnapshotReaderStopped    SnapshotReaderLifecycle = "stopped"
)

// SnapshotReaderActorStatus is the public status value published by the supervisor.
// Reader contains the independently managed broker-reader meta-process state.
type SnapshotReaderActorStatus struct {
	Lifecycle    SnapshotReaderLifecycle
	Availability runtime.Availability
	Generation   int64
	Reader       SnapshotReaderMetaStatus
}

type snapshotReaderMetaState struct {
	alias   gen.Alias
	restart *runtime.ScheduledBackoff
	status  SnapshotReaderMetaStatus
}

type snapshotReaderActor struct {
	act.Actor
	opts               SnapshotReaderSupervisorOptions
	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref
	reader             snapshotReaderMetaState
	entries            map[string]snapshot.EffectiveEntry
	committed          *snapshot.Snapshot
}

// --- messages ---

type MessageSnapshotReaderRestart struct{ token uint64 }

type MessageSnapshotReaderActivate struct {
	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref
}

// --- messages ---

// Init initializes reader state and its restart backoff.
func (a *snapshotReaderActor) Init(...any) error {
	a.entries = make(map[string]snapshot.EffectiveEntry)
	a.reader.status = SnapshotReaderMetaStatus{
		Lifecycle:    SnapshotReaderMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	a.reader.restart = runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax)
	return nil
}

// HandleMessage processes reader records, lifecycle, and restart messages.
func (a *snapshotReaderActor) HandleMessage(_ gen.PID, message any) error {
	defer a.reportStatus()

	switch m := message.(type) {
	case MessageSnapshotReaderActivate:
		if a.snapshotEventToken != (gen.Ref{}) {
			return fmt.Errorf("snapshot reader actor already activated")
		}
		a.snapshotEventName = m.snapshotEventName
		a.snapshotEventToken = m.snapshotEventToken
		return a.startSnapshotReaderMeta()

	case MessageSnapshotRecord:
		if m.source != a.reader.alias || a.reader.alias == (gen.Alias{}) {
			return nil
		}
		started := a.reader.status.Lifecycle != SnapshotReaderMetaRunning
		if started {
			a.reader.status.Lifecycle = SnapshotReaderMetaRunning
			a.reader.status.Availability = runtime.AvailabilityDegraded
			a.reader.status.CaughtUp = false
			a.reader.status.LastError = nil
		}
		committed := a.apply(m.message)
		if committed && a.reader.status.CaughtUp {
			a.reader.status.Availability = runtime.AvailabilityReady
			a.reader.status.LastError = nil
			a.publishSnapshot()
		}

	case MessageSnapshotCaughtUp:
		if m.source != a.reader.alias ||
			a.reader.alias == (gen.Alias{}) ||
			a.reader.status.CaughtUp {
			return nil
		}
		a.reader.status.Lifecycle = SnapshotReaderMetaRunning
		a.reader.status.Availability = runtime.AvailabilityReady
		a.reader.status.CaughtUp = true
		a.reader.restart.CancelScheduled(true)
		if a.committed == nil {
			a.reader.status.Availability = runtime.AvailabilityUnavailable
			a.reader.status.LastError = fmt.Errorf("%w: generation marker not found", runtime.ErrSnapshotRead)
			return nil
		}
		a.reader.status.LastError = nil
		a.publishSnapshot()

	case MessageSnapshotReaderRestart:
		if !a.reader.restart.Pending ||
			a.reader.restart.Token != m.token ||
			a.reader.alias != (gen.Alias{}) {
			return nil
		}
		a.reader.restart.Pending = false
		a.reader.restart.Cancel = nil
		return a.startSnapshotReaderMeta()

	case gen.MessageDownAlias:
		if m.Alias != a.reader.alias {
			return nil
		}
		a.reader.alias = gen.Alias{}
		a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
		a.reader.status.Availability = runtime.AvailabilityUnavailable
		a.reader.status.CaughtUp = false
		a.reader.status.LastError = m.Reason
		a.opts.Logger.ErrorF("snapshot reader actor: reader %s stopped: %v", m.Alias, m.Reason)
		return a.scheduleSnapshotReaderMetaRestart()
	}
	return nil
}

// HandleCall rejects unsupported synchronous requests.
func (a *snapshotReaderActor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot reader actor: unsupported call %T", request), nil
}

// Terminate stops the reader meta-process and marks it unavailable.
func (a *snapshotReaderActor) Terminate(error) {
	defer a.reportStatus()
	a.reader.restart.CancelScheduled(false)
	a.stopSnapshotReaderMeta(gen.TerminateReasonShutdown)
	a.reader.status.Lifecycle = SnapshotReaderMetaStopped
	a.reader.status.Availability = runtime.AvailabilityUnavailable
	a.reader.status.CaughtUp = false
}

// startSnapshotReaderMeta starts compacted-topic reconstruction while retaining the prior published snapshot.
func (a *snapshotReaderActor) startSnapshotReaderMeta() error {
	a.stopSnapshotReaderMeta(gen.TerminateReasonShutdown)
	a.reader.status = SnapshotReaderMetaStatus{
		Lifecycle:    SnapshotReaderMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	a.entries = make(map[string]snapshot.EffectiveEntry)
	a.committed = nil

	reader := a.opts.ReaderFactory()
	if reader == nil {
		a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
		a.reader.status.LastError = fmt.Errorf("start snapshot reader meta: reader factory returned nil")
		return a.scheduleSnapshotReaderMetaRestart()
	}

	alias, err := a.SpawnMeta(&snapshotReaderMeta{reader: reader}, gen.MetaOptions{})
	if err != nil {
		_ = reader.Close()
		a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
		a.reader.status.LastError = fmt.Errorf("spawn snapshot reader meta: %w", err)
		return a.scheduleSnapshotReaderMetaRestart()
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
		a.reader.status.LastError = fmt.Errorf("monitor snapshot reader meta: %w", err)
		return a.scheduleSnapshotReaderMetaRestart()
	}
	a.reader.alias = alias
	return nil
}

// stopSnapshotReaderMeta stops the active reader meta-process.
func (a *snapshotReaderActor) stopSnapshotReaderMeta(reason error) {
	if a.reader.alias == (gen.Alias{}) {
		return
	}
	alias := a.reader.alias
	a.reader.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

// scheduleSnapshotReaderMetaRestart schedules a backoff-delayed reader restart.
func (a *snapshotReaderActor) scheduleSnapshotReaderMetaRestart() error {
	if a.reader.restart.Pending {
		return nil
	}
	delay := a.reader.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot reader meta restart: %w", runtime.ErrBackoffStopped)
	}
	a.reader.restart.Token++
	token := a.reader.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageSnapshotReaderRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule snapshot reader meta restart: %w", err)
	}
	a.reader.restart.Pending = true
	a.reader.restart.Cancel = cancel
	a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
	a.reader.status.Availability = runtime.AvailabilityUnavailable
	return nil
}

// apply incorporates a compacted-topic record and reports a new committed generation.
func (a *snapshotReaderActor) apply(message brokers.Message) bool {
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
func (a *snapshotReaderActor) publishSnapshot() {
	if a.committed == nil {
		return
	}
	if a.snapshotEventToken != (gen.Ref{}) {
		_ = a.SendEvent(a.snapshotEventName, a.snapshotEventToken, a.committed.Clone())
	}
}

// sortedEntries returns cloned entries ordered by identifier.
func (a *snapshotReaderActor) sortedEntries() []snapshot.EffectiveEntry {
	entries := make([]snapshot.EffectiveEntry, 0, len(a.entries))
	for _, entry := range a.entries {
		entries = append(entries, entry.Clone())
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Id < entries[j].Id })
	return entries
}

// reportStatus sends current reader status to the supervisor.
func (a *snapshotReaderActor) reportStatus() {
	if a.snapshotEventToken == (gen.Ref{}) {
		return
	}

	availability := runtime.AvailabilityUnavailable
	switch a.reader.status.Availability {
	case runtime.AvailabilityReady:
		availability = runtime.AvailabilityReady
	case runtime.AvailabilityDegraded:
		availability = runtime.AvailabilityDegraded
	}

	status := SnapshotReaderActorStatus{
		Lifecycle:    SnapshotReaderRunning,
		Availability: availability,
		Reader:       a.reader.status,
	}
	if a.committed != nil {
		status.Generation = a.committed.Generation
	}
	_ = a.Send(a.Parent(), MessageSnapshotReaderStatusChanged{status: status})
}

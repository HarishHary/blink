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
	LastError    error
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

	opts SnapshotReaderSupervisorOptions

	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref

	reader snapshotReaderMetaState

	entries   map[string]snapshot.EffectiveEntry
	committed *snapshot.Snapshot
}

// --- messages ---

type MessageSnapshotReaderRestart struct{ token uint64 }

type MessageSnapshotReaderActivate struct {
	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref
}

// --- messages ---

func (a *snapshotReaderActor) Init(...any) error {
	a.entries = make(map[string]snapshot.EffectiveEntry)
	a.reader.status = SnapshotReaderMetaStatus{
		Lifecycle:    SnapshotReaderMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	a.reader.restart = runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax)
	return nil
}

func (a *snapshotReaderActor) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case MessageSnapshotReaderActivate:
		if a.snapshotEventToken != (gen.Ref{}) {
			return fmt.Errorf("snapshot reader actor already activated")
		}
		a.snapshotEventName = m.snapshotEventName
		a.snapshotEventToken = m.snapshotEventToken
		a.publishStatus()
		return a.startSnapshotReaderMeta()

	case MessageSnapshotRecord:
		if m.incarnation != a.reader.status.Incarnation || a.reader.alias == (gen.Alias{}) {
			return nil
		}
		started := a.reader.status.Lifecycle != SnapshotReaderMetaRunning
		if started {
			a.reader.status.Lifecycle = SnapshotReaderMetaRunning
			a.reader.status.Availability = runtime.AvailabilityDegraded
			a.reader.status.CaughtUp = false
			a.reader.status.RestartPending = false
			a.reader.status.LastError = nil
		}
		committed := a.apply(m.message)
		if committed && a.reader.status.CaughtUp {
			a.reader.status.Availability = runtime.AvailabilityReady
			a.reader.status.LastError = nil
			a.publishSnapshot()
			a.publishStatus()
		} else if started {
			a.publishStatus()
		}

	case MessageSnapshotCaughtUp:
		if m.incarnation != a.reader.status.Incarnation ||
			a.reader.alias == (gen.Alias{}) ||
			a.reader.status.CaughtUp {
			return nil
		}
		a.reader.status.Lifecycle = SnapshotReaderMetaRunning
		a.reader.status.Availability = runtime.AvailabilityReady
		a.reader.status.CaughtUp = true
		a.reader.status.RestartPending = false
		a.reader.restart.CancelScheduled(true)
		if a.committed == nil {
			a.reader.status.Availability = runtime.AvailabilityUnavailable
			a.reader.status.LastError = fmt.Errorf("%w: generation marker not found", runtime.ErrSnapshotRead)
			a.publishStatus()
			return nil
		}
		a.reader.status.LastError = nil
		a.publishSnapshot()
		a.publishStatus()

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
		a.opts.Logger.ErrorF(
			"snapshot reader actor: reader incarnation %d stopped: %v",
			a.reader.status.Incarnation,
			m.Reason,
		)
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}
	return nil
}

func (a *snapshotReaderActor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot reader actor: unsupported call %T", request), nil
}

func (a *snapshotReaderActor) Terminate(error) {
	a.reader.restart.CancelScheduled(false)
	a.stopSnapshotReaderMeta(gen.TerminateReasonShutdown)
	a.reader.status.Lifecycle = SnapshotReaderMetaStopped
	a.reader.status.Availability = runtime.AvailabilityUnavailable
	a.reader.status.CaughtUp = false
}

// startSnapshotReaderMeta starts a fresh reader incarnation and a fresh compacted
// topic reconstruction. The previous complete buffered snapshot remains
// available to subscribers while this incarnation catches up.
func (a *snapshotReaderActor) startSnapshotReaderMeta() error {
	a.stopSnapshotReaderMeta(gen.TerminateReasonShutdown)
	restartCount := a.reader.status.RestartCount
	if a.reader.status.Incarnation > 0 {
		restartCount++
	}
	incarnation := a.reader.status.Incarnation + 1
	a.reader.status = SnapshotReaderMetaStatus{
		Lifecycle:    SnapshotReaderMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
		Incarnation:  incarnation,
		RestartCount: restartCount,
	}
	a.entries = make(map[string]snapshot.EffectiveEntry)
	a.committed = nil
	a.publishStatus()

	reader := a.opts.ReaderFactory()
	if reader == nil {
		a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
		a.reader.status.LastError = fmt.Errorf("start snapshot reader meta: reader factory returned nil")
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}

	alias, err := a.SpawnMeta(&snapshotReaderMeta{reader: reader, incarnation: incarnation}, gen.MetaOptions{})
	if err != nil {
		_ = reader.Close()
		a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
		a.reader.status.LastError = fmt.Errorf("spawn snapshot reader meta: %w", err)
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.reader.status.Lifecycle = SnapshotReaderMetaRestarting
		a.reader.status.LastError = fmt.Errorf("monitor snapshot reader meta: %w", err)
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}
	a.reader.alias = alias
	return nil
}

func (a *snapshotReaderActor) stopSnapshotReaderMeta(reason error) {
	if a.reader.alias == (gen.Alias{}) {
		return
	}
	alias := a.reader.alias
	a.reader.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

func (a *snapshotReaderActor) scheduleSnapshotReaderMetaRestart() error {
	if a.reader.restart.Pending {
		return nil
	}

	delay := a.reader.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot reader meta restart backoff stopped")
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
	a.reader.status.RestartPending = true
	a.publishStatus()
	return nil
}

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

func (a *snapshotReaderActor) publishSnapshot() {
	if a.committed == nil {
		return
	}
	if a.snapshotEventToken != (gen.Ref{}) {
		_ = a.SendEvent(a.snapshotEventName, a.snapshotEventToken, a.committed.Clone())
	}
}

func (a *snapshotReaderActor) sortedEntries() []snapshot.EffectiveEntry {
	entries := make([]snapshot.EffectiveEntry, 0, len(a.entries))
	for _, entry := range a.entries {
		entries = append(entries, entry.Clone())
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Id < entries[j].Id })
	return entries
}

func (a *snapshotReaderActor) publishStatus() {
	if a.snapshotEventToken == (gen.Ref{}) {
		return
	}
	_ = a.Send(a.Parent(), MessageSnapshotReaderStatusChanged{status: a.currentStatus()})
}

func (a *snapshotReaderActor) currentStatus() SnapshotReaderActorStatus {
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
	return status
}

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

type snapshotEntry = snapshot.EffectiveEntry

// SnapshotReaderMetaLifecycle describes one blocking broker-reader
// meta-process incarnation.
type SnapshotReaderMetaLifecycle string

const (
	SnapshotReaderMetaStarting   SnapshotReaderMetaLifecycle = "starting"
	SnapshotReaderMetaRunning    SnapshotReaderMetaLifecycle = "running"
	SnapshotReaderMetaRestarting SnapshotReaderMetaLifecycle = "restarting"
	SnapshotReaderMetaStopped    SnapshotReaderMetaLifecycle = "stopped"
)

// SnapshotReaderMetaStatus is owned by snapshotReaderActor because that actor
// owns the meta-process generation, restart policy, and interpretation of
// MessageDownAlias. The meta-process only reports runtime facts.
type SnapshotReaderMetaStatus struct {
	Lifecycle      SnapshotReaderMetaLifecycle
	Availability   runtime.Availability
	Generation     uint64
	CaughtUp       bool
	RestartPending bool
	LastError      string
}

// snapshotReaderMetaRestart is owned by the actor because the actor owns
// creation and restart policy for its blocking reader meta-process.
type snapshotReaderMetaRestart struct{ token uint64 }

type snapshotReaderActor struct {
	act.Actor

	opts Options

	actorGeneration uint64

	revision int64

	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref

	activated bool

	readerAlias          gen.Alias
	readerStatus         SnapshotReaderMetaStatus
	readerRestartBackoff *backoff.ExponentialBackOff
	readerRestartCancel  gen.CancelFunc
	readerRestartToken   uint64

	entries           map[string]snapshot.EffectiveEntry
	localRevision     int64
	appliedGeneration int64
}

func (a *snapshotReaderActor) Init(...any) error {
	a.entries = make(map[string]snapshotEntry)
	a.readerStatus = SnapshotReaderMetaStatus{
		Lifecycle:    SnapshotReaderMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	a.readerRestartBackoff = backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(a.opts.RestartMin),
		backoff.WithMaxInterval(a.opts.RestartMax),
		backoff.WithMultiplier(2),
		backoff.WithMaxElapsedTime(0),
	)
	return nil
}

func (a *snapshotReaderActor) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case snapshotReaderActorActivate:
		if m.generation <= a.actorGeneration {
			return nil
		}
		if a.activated {
			return fmt.Errorf("snapshot reader actor already activated as generation %d", a.actorGeneration)
		}
		a.actorGeneration = m.generation
		a.revision = m.revision
		a.snapshotEventName = m.snapshotEventName
		a.snapshotEventToken = m.snapshotEventToken
		a.activated = true
		a.publishStatus()
		return a.startSnapshotReaderMeta()

	case snapshotReaderMetaStarted:
		if m.generation != a.readerStatus.Generation || a.readerAlias == (gen.Alias{}) {
			return nil
		}
		a.readerStatus.Lifecycle = SnapshotReaderMetaRunning
		a.readerStatus.Availability = runtime.AvailabilityDegraded
		a.readerStatus.CaughtUp = false
		a.readerStatus.RestartPending = false
		a.readerStatus.LastError = ""
		a.publishStatus()

	case snapshotReaderMetaRecord:
		if m.generation != a.readerStatus.Generation {
			return nil
		}
		changed := a.apply(m.message)
		if changed && a.readerStatus.CaughtUp {
			a.publishSnapshot()
		}
		if a.readerStatus.CaughtUp {
			a.publishStatus()
		}

	case snapshotReaderMetaCaughtUp:
		if m.generation != a.readerStatus.Generation || a.readerStatus.CaughtUp {
			return nil
		}
		a.readerStatus.Lifecycle = SnapshotReaderMetaRunning
		a.readerStatus.Availability = runtime.AvailabilityReady
		a.readerStatus.CaughtUp = true
		a.readerStatus.RestartPending = false
		a.readerStatus.LastError = ""
		a.publishSnapshot()
		a.resetSnapshotReaderMetaRestartBackoff()
		a.publishStatus()

	case snapshotReaderMetaRestart:
		if !a.readerStatus.RestartPending ||
			a.readerRestartToken != m.token ||
			a.readerAlias != (gen.Alias{}) {
			return nil
		}
		a.readerStatus.RestartPending = false
		a.readerRestartCancel = nil
		return a.startSnapshotReaderMeta()

	case gen.MessageDownAlias:
		if m.Alias != a.readerAlias {
			return nil
		}
		a.readerAlias = gen.Alias{}
		a.readerStatus.Lifecycle = SnapshotReaderMetaRestarting
		a.readerStatus.Availability = runtime.AvailabilityUnavailable
		a.readerStatus.CaughtUp = false
		a.readerStatus.LastError = errorText(m.Reason)
		a.opts.Logger.ErrorF(
			"snapshot reader actor: reader generation %d stopped: %v",
			a.readerStatus.Generation,
			m.Reason,
		)
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}
	return nil
}

func (a *snapshotReaderActor) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (a *snapshotReaderActor) Terminate(error) {
	a.cancelSnapshotReaderMetaRestart(false)
	a.stopSnapshotReaderMeta(gen.TerminateReasonShutdown)
	a.readerStatus.Lifecycle = SnapshotReaderMetaStopped
	a.readerStatus.Availability = runtime.AvailabilityUnavailable
	a.readerStatus.CaughtUp = false
}

// startSnapshotReaderMeta starts a fresh reader incarnation and a fresh compacted
// topic reconstruction. The previous complete buffered snapshot remains
// available to subscribers while this generation catches up.
func (a *snapshotReaderActor) startSnapshotReaderMeta() error {
	a.stopSnapshotReaderMeta(gen.TerminateReasonShutdown)
	a.readerStatus.Generation++
	a.readerStatus.Lifecycle = SnapshotReaderMetaStarting
	a.readerStatus.Availability = runtime.AvailabilityUnavailable
	a.readerStatus.CaughtUp = false
	a.readerStatus.RestartPending = false
	a.entries = make(map[string]snapshotEntry)
	a.appliedGeneration = 0
	a.publishStatus()

	reader := a.opts.ReaderFactory()
	if reader == nil {
		a.readerStatus.Lifecycle = SnapshotReaderMetaRestarting
		a.readerStatus.LastError = "reader factory returned nil"
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}

	alias, err := a.SpawnMeta(&snapshotReaderMeta{reader: reader, generation: a.readerStatus.Generation}, gen.MetaOptions{})
	if err != nil {
		_ = reader.Close()
		a.readerStatus.Lifecycle = SnapshotReaderMetaRestarting
		a.readerStatus.LastError = fmt.Sprintf("spawn snapshot reader meta: %v", err)
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.readerStatus.Lifecycle = SnapshotReaderMetaRestarting
		a.readerStatus.LastError = fmt.Sprintf("monitor snapshot reader meta: %v", err)
		a.publishStatus()
		return a.scheduleSnapshotReaderMetaRestart()
	}
	a.readerAlias = alias
	return nil
}

func (a *snapshotReaderActor) stopSnapshotReaderMeta(reason error) {
	if a.readerAlias == (gen.Alias{}) {
		return
	}
	alias := a.readerAlias
	a.readerAlias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

func (a *snapshotReaderActor) scheduleSnapshotReaderMetaRestart() error {
	if a.readerStatus.RestartPending {
		return nil
	}

	delay := a.readerRestartBackoff.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot reader meta restart backoff stopped")
	}

	a.readerRestartToken++
	token := a.readerRestartToken
	cancel, err := a.SendAfter(a.PID(), snapshotReaderMetaRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule snapshot reader meta restart: %w", err)
	}
	a.readerStatus.Lifecycle = SnapshotReaderMetaRestarting
	a.readerStatus.Availability = runtime.AvailabilityUnavailable
	a.readerStatus.RestartPending = true
	a.readerRestartCancel = cancel
	a.publishStatus()
	return nil
}

func (a *snapshotReaderActor) cancelSnapshotReaderMetaRestart(reset bool) {
	if a.readerRestartCancel != nil {
		a.readerRestartCancel()
		a.readerRestartCancel = nil
	}
	a.readerStatus.RestartPending = false
	a.readerRestartToken++
	if reset {
		a.readerRestartBackoff.Reset()
	}
}

func (a *snapshotReaderActor) resetSnapshotReaderMetaRestartBackoff() {
	a.cancelSnapshotReaderMetaRestart(true)
}

func (a *snapshotReaderActor) apply(message brokers.Message) bool {
	key := string(message.Key)
	if key == snapshot.GenerationMarkerKey {
		generation, err := snapshot.DecodeGeneration(message.Value)
		if err != nil {
			a.opts.Logger.ErrorF("snapshot reader actor: decode generation marker: %v", err)
			return false
		}
		if generation < a.appliedGeneration {
			a.opts.Logger.ErrorF(
				"snapshot reader actor: generation went backwards %d -> %d",
				a.appliedGeneration,
				generation,
			)
		}
		a.appliedGeneration = generation
		return false
	}

	if len(message.Value) == 0 {
		if _, ok := a.entries[key]; !ok {
			return false
		}
		delete(a.entries, key)
		return true
	}

	entry, err := snapshot.Unmarshal(message.Value)
	if err != nil {
		a.opts.Logger.ErrorF("snapshot reader actor: unmarshal entry %q: %v", key, err)
		return false
	}
	a.entries[entry.Id] = entry
	return true
}

func (a *snapshotReaderActor) publishSnapshot() {
	entries := make([]snapshot.EffectiveEntry, 0, len(a.entries))
	for _, entry := range a.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Id < entries[j].Id })

	a.localRevision++
	published := &snapshot.Snapshot{
		Generation: a.revision + a.localRevision,
		Entries:    entries,
	}
	if a.snapshotEventToken != (gen.Ref{}) {
		_ = a.SendEvent(a.snapshotEventName, a.snapshotEventToken, published)
	}
}

func (a *snapshotReaderActor) publishStatus() {
	if !a.activated {
		return
	}
	_ = a.Send(a.Parent(), snapshotReaderActorStatusChanged{
		pid:        a.PID(),
		generation: a.actorGeneration,
		status:     a.currentStatus(),
	})
}

func (a *snapshotReaderActor) currentStatus() SnapshotReaderStatus {
	availability := runtime.AvailabilityUnavailable
	switch a.readerStatus.Availability {
	case runtime.AvailabilityReady:
		availability = runtime.AvailabilityReady
	case runtime.AvailabilityDegraded:
		availability = runtime.AvailabilityDegraded
	}

	return SnapshotReaderStatus{
		Lifecycle:         SnapshotReaderRunning,
		Availability:      availability,
		ActorGeneration:   a.actorGeneration,
		LocalRevision:     a.localRevision,
		AppliedGeneration: a.appliedGeneration,
		Reader:            a.readerStatus,
	}
}

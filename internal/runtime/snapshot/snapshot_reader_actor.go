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

const subscribeTimeoutSeconds = 5

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

type MessageReaderActorStatusChanged struct{ status ReaderActorStatus }

// SubscribeRequest asks for the current committed snapshot and registers the caller (its own PID, delivered as HandleCall's "from", not a field here) for future pushed SnapshotUpdate commits.
type SubscribeRequest struct {
	ExecutorID      string
	KnownGeneration int64
	Role            string
}

// SubscribeResponse answers SubscribeRequest; ControllerPID lets the caller Monitor it without a second lookup, and future commits arrive as pushed SnapshotUpdate messages since a channel can't cross the cluster.
type SubscribeResponse struct {
	Current       *snapshot.Snapshot
	Changes       []snapshot.EntryChange
	ControllerPID gen.PID
}

// SnapshotUpdate is one commit's full state, pushed to every subscriber; Changes/Tombstones are for observability only - applying it just needs Snapshot (see readerActor).
type SnapshotUpdate struct {
	Snapshot   *snapshot.Snapshot
	Changes    []snapshot.EntryChange
	Tombstones []string
}

// UnsubscribeRequest stops future pushes to ExecutorID; best-effort only - MonitorPID on the executor's PID is the authoritative removal path.
type UnsubscribeRequest struct{ ExecutorID string }

// ExecutorHeartbeat is this reader's periodic liveness/generation report to the controller.
type ExecutorHeartbeat struct {
	CommittedGeneration int64
	ReadyGeneration     int64
	Availability        string
}

// ExecutorAppliedGeneration reports that this reader's owning plugin runtime reached a generation.
type ExecutorAppliedGeneration struct {
	Generation int64
	Admitted   bool
}

// MessageExecutorReport carries this reader's convergence report (Heartbeat and/or AppliedGeneration, either may be nil), sent fire-and-forget to the controller actor.
type MessageExecutorReport struct {
	ExecutorID string
	Heartbeat  *ExecutorHeartbeat
	Applied    *ExecutorAppliedGeneration
	LastError  string
}

type MessageSubscribeRestart struct{ token uint64 }

// --- messages ---

// readerActor issues one bounded Call to subscribe, then passively receives pushed SnapshotUpdate messages - Ergo remote delivery is push-based, so there's no read loop or meta process to supervise.
type readerActor struct {
	act.Actor
	opts ReaderActorOptions

	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref

	controllerPID  gen.PID
	subscribed     bool
	lastGeneration int64
	lastError      error
	restart        *runtime.ScheduledBackoff
}

// newReaderActor constructs the reader actor for one subscription.
func newReaderActor(opts ReaderActorOptions) gen.ProcessBehavior {
	return &readerActor{opts: opts}
}

// Init initializes the resubscribe backoff.
func (a *readerActor) Init(...any) error {
	a.restart = runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax)
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
		a.restart.Pending = false
		a.restart.Cancel = nil
		return a.subscribe()
	case gen.MessageDownPID:
		if !a.subscribed || m.PID != a.controllerPID {
			return nil
		}
		a.controllerPID = gen.PID{}
		a.subscribed = false
		a.lastError = m.Reason
		a.Log().Error("snapshot reader actor: controller %s stopped: %v", m.PID, m.Reason)
		return a.scheduleSubscribeRestart()
	case gen.MessageDownNode:
		if !a.subscribed || m.Name != a.opts.Endpoint.Node {
			return nil
		}
		a.controllerPID = gen.PID{}
		a.subscribed = false
		a.lastError = fmt.Errorf("controller node %s down", m.Name)
		a.Log().Error("snapshot reader actor: controller node %s down", m.Name)
		return a.scheduleSubscribeRestart()
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
	a.restart.CancelScheduled(false)
	if a.subscribed {
		_ = a.SendProcessID(a.opts.Endpoint, UnsubscribeRequest{ExecutorID: a.opts.ExecutorID})
	}
	a.controllerPID = gen.PID{}
	a.subscribed = false
}

// subscribe issues a bounded Call to the controller, then monitors it for loss detection.
func (a *readerActor) subscribe() error {
	request := SubscribeRequest{
		ExecutorID:      a.opts.ExecutorID,
		KnownGeneration: a.lastGeneration,
		Role:            a.opts.Role,
	}
	response, err := a.CallProcessID(a.opts.Endpoint, request, subscribeTimeoutSeconds)
	if err != nil {
		a.lastError = fmt.Errorf("%w: subscribe: %w", runtime.ErrSnapshotSubscribe, err)
		return a.scheduleSubscribeRestart()
	}
	sub, ok := response.(SubscribeResponse)
	if !ok {
		a.lastError = fmt.Errorf("%w: subscribe: unexpected response %T", runtime.ErrSnapshotSubscribe, response)
		return a.scheduleSubscribeRestart()
	}
	if err := a.MonitorPID(sub.ControllerPID); err != nil {
		a.lastError = fmt.Errorf("%w: monitor controller: %w", runtime.ErrSnapshotSubscribe, err)
		return a.scheduleSubscribeRestart()
	}
	_ = a.MonitorNode(a.opts.Endpoint.Node)

	a.restart.CancelScheduled(true)
	a.controllerPID = sub.ControllerPID
	a.subscribed = true
	a.lastError = nil
	if sub.Current != nil && sub.Current.Generation > a.lastGeneration {
		a.lastGeneration = sub.Current.Generation
		a.publishSnapshot(sub.Current)
	}
	a.readerMeta.alias = alias
	return nil
}

// scheduleSubscribeRestart schedules a backoff-delayed resubscribe.
func (a *readerActor) scheduleSubscribeRestart() error {
	if a.restart.Pending {
		return nil
	}
	delay := a.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot reader restart: %w", runtime.ErrBackoffStopped)
	}
	a.restart.Token++
	token := a.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageSubscribeRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule snapshot reader restart: %w", err)
	}
	a.restart.Pending = true
	a.restart.Cancel = cancel
	return nil
}

// publishSnapshot sends the received snapshot to subscribers.
func (a *readerActor) publishSnapshot(snap *snapshot.Snapshot) {
	if a.snapshotEventToken == (gen.Ref{}) {
		return
	}
	_ = a.SendEvent(a.snapshotEventName, a.snapshotEventToken, snap.Clone())
}

// reportStatus sends current reader status to the supervisor.
func (a *readerActor) reportStatus() {
	if a.snapshotEventToken == (gen.Ref{}) {
		return
	}

	availability := runtime.AvailabilityUnavailable
	if a.subscribed {
		availability = runtime.AvailabilityReady
	case runtime.AvailabilityDegraded:
		availability = runtime.AvailabilityDegraded
	}
	_ = a.Send(a.Parent(), MessageReaderActorStatusChanged{status: ReaderActorStatus{
		Lifecycle:    ReaderActorRunning,
		Availability: availability,
		Generation:   a.lastGeneration,
	}})
}

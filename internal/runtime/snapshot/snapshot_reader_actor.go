package snapshot

import (
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
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
	LastError    string
}

// --- messages ---

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
	Current       *Snapshot
	Changes       []EntryChange
	ControllerPID gen.PID
}

// SnapshotUpdate is one commit's full state, pushed to every subscriber; Changes/Tombstones are for observability only - applying it just needs Snapshot (see readerActor).
type SnapshotUpdate struct {
	Snapshot   *Snapshot
	Changes    []EntryChange
	Tombstones []string
}

// UnsubscribeRequest stops future pushes to ExecutorID; best-effort only - MonitorPID on the executor's PID is the authoritative removal path.
type UnsubscribeRequest struct{ ExecutorID string }

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
	lastStatus     ReaderActorStatus
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

// HandleMessage processes activation, pushed snapshot updates, controller-loss, and restart messages.
func (a *readerActor) HandleMessage(from gen.PID, message any) error {
	defer a.reconcileStatus()
	switch m := message.(type) {
	case MessageReaderActorActivate:
		if from != a.Parent() || a.snapshotEventToken != (gen.Ref{}) {
			return nil
		}
		a.snapshotEventName = m.snapshotEventName
		a.snapshotEventToken = m.snapshotEventToken
		return a.subscribe()
	case SnapshotUpdate:
		if !a.subscribed || from != a.controllerPID || m.Snapshot == nil || m.Snapshot.Generation <= a.lastGeneration {
			return nil
		}
		a.lastGeneration = m.Snapshot.Generation
		a.lastError = nil
		a.publishSnapshot(m.Snapshot)
		return nil
	case MessageSubscribeRestart:
		if !a.restart.Pending || a.restart.Token != m.token || a.subscribed {
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

// Terminate cancels any pending resubscribe and notifies the controller, best effort.
func (a *readerActor) Terminate(error) {
	defer a.reconcileStatus()
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
func (a *readerActor) publishSnapshot(snap *Snapshot) {
	if a.snapshotEventToken == (gen.Ref{}) {
		return
	}
	_ = a.SendEvent(a.snapshotEventName, a.snapshotEventToken, snap.Clone())
}

// reconcileStatus recomputes and, on change, sends the current reader status to the supervisor.
func (a *readerActor) reconcileStatus() {
	if a.snapshotEventToken == (gen.Ref{}) {
		return
	}
	next := a.status()
	if next == a.lastStatus {
		return
	}
	a.lastStatus = next
	_ = a.Send(a.Parent(), MessageReaderActorStatusChanged{status: next})
}

// status derives the reader's current publishable status, shared by reconcileStatus (to the supervisor) and HandleInspect (to an operator).
func (a *readerActor) status() ReaderActorStatus {
	availability := runtime.AvailabilityUnavailable
	if a.subscribed {
		availability = runtime.AvailabilityReady
	}
	status := ReaderActorStatus{
		Lifecycle:    ReaderActorRunning,
		Availability: availability,
		Generation:   a.lastGeneration,
	}
	if a.lastError != nil {
		status.LastError = a.lastError.Error()
	}
	return status
}

// HandleInspect exposes concise reader operational state: whether it's currently subscribed, why
// not if it isn't, the last generation it received, and which controller it's subscribed to.
func (a *readerActor) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	return map[string]string{
		"reader:availability": string(status.Availability),
		"reader:subscribed":   fmt.Sprintf("%t", a.subscribed),
		"reader:generation":   fmt.Sprintf("%d", a.lastGeneration),
		"reader:controller":   fmt.Sprintf("%s", a.controllerPID),
		"reader:executor_id":  a.opts.ExecutorID,
		"reader:last_error":   status.LastError,
	}
}

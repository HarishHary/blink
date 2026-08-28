package snapshot

import (
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
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

// MessageReaderActorActivate tells the reader child its parent recorded its PID and it may subscribe.
type MessageReaderActorActivate struct{}

type MessageReaderActorStatusChanged struct{ status ReaderActorStatus }

// SubscribeRequest asks for the committed snapshot and registers the caller for pushed SnapshotUpdate
// commits; its PID arrives as HandleCall's "from" rather than a field here.
type SubscribeRequest struct {
	ExecutorID      string
	KnownGeneration int64
}

// SubscribeResponse answers SubscribeRequest; ControllerPID saves the caller a lookup before it
// monitors, and later commits arrive as messages since a channel cannot cross the cluster.
type SubscribeResponse struct {
	Current       *Snapshot
	Changes       []EntryChange
	ControllerPID gen.PID
}

// SnapshotUpdate is one commit's full state, pushed to every subscriber; Changes/Tombstones are for
// observability only - applying it just needs Snapshot (see readerActor).
type SnapshotUpdate struct {
	Snapshot   *Snapshot
	Changes    []EntryChange
	Tombstones []string
}

// UnsubscribeRequest stops future pushes to ExecutorID; best-effort only - MonitorPID on the
// executor's PID is the authoritative removal path.
type UnsubscribeRequest struct{ ExecutorID string }

type MessageSubscribeRestart struct{ token uint64 }

// --- messages ---

// readerActor makes one bounded Call to subscribe and then receives pushed SnapshotUpdate messages;
// Ergo remote delivery is push-based, so there is no read loop or meta to supervise.
type readerActor struct {
	act.Actor
	opts           ReaderActorOptions
	labels         telemetry.Labels
	snapshotEvent  eventPublication
	activated      bool
	controllerPID  gen.PID
	subscribed     bool
	lastGeneration int64
	lastError      error
	restart        *runtime.ScheduledBackoff
	lastStatus     ReaderActorStatus
}

// newReaderActor constructs the reader for one subscription; it publishes every snapshot it
// receives, so it takes the publication its supervisor registered for it.
func newReaderActor(opts ReaderActorOptions, labels telemetry.Labels, snapshotEvent eventPublication) gen.ProcessBehavior {
	return &readerActor{opts: opts, labels: labels, snapshotEvent: snapshotEvent}
}

// Init validates the snapshot publication and initializes the resubscribe backoff.
func (a *readerActor) Init(...any) error {
	if !a.snapshotEvent.registered() {
		return fmt.Errorf("snapshot reader: a registered snapshot event is required")
	}
	a.restart = runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax)
	return nil
}

// HandleMessage processes activation, pushed snapshot updates, controller-loss, and restart messages.
func (a *readerActor) HandleMessage(from gen.PID, message any) error {
	defer a.reconcileStatus()
	switch m := message.(type) {
	case MessageReaderActorActivate:
		if from != a.Parent() || a.activated {
			return nil
		}
		a.activated = true
		return a.subscribe()
	case SnapshotUpdate:
		if reason := a.updateRejection(from, m); reason != "" {
			a.labels.Count(a, metricUpdatesIgnored, reason)
			return nil
		}
		a.labels.Count(a, metricUpdates)
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
		a.labels.Count(a, metricControllerDown, "process")
		a.controllerPID = gen.PID{}
		a.subscribed = false
		a.lastError = m.Reason
		a.Log().Error("snapshot reader actor: controller %s stopped: %v", m.PID, m.Reason)
		return a.scheduleSubscribeRestart()
	case gen.MessageDownNode:
		if !a.subscribed || m.Name != a.opts.Endpoint.Node {
			return nil
		}
		a.labels.Count(a, metricControllerDown, "node")
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
	}
	response, err := a.CallProcessID(a.opts.Endpoint, request, subscribeTimeoutSeconds)
	if err != nil {
		a.labels.Count(a, metricSubscribeAttempts, "unreachable")
		a.lastError = fmt.Errorf("%w: subscribe: %w", runtime.ErrSnapshotSubscribe, err)
		return a.scheduleSubscribeRestart()
	}
	sub, ok := response.(SubscribeResponse)
	if !ok {
		a.labels.Count(a, metricSubscribeAttempts, "bad_response")
		a.lastError = fmt.Errorf("%w: subscribe: unexpected response %T", runtime.ErrSnapshotSubscribe, response)
		return a.scheduleSubscribeRestart()
	}
	if err := a.MonitorPID(sub.ControllerPID); err != nil {
		a.labels.Count(a, metricSubscribeAttempts, "unmonitorable")
		a.lastError = fmt.Errorf("%w: monitor controller: %w", runtime.ErrSnapshotSubscribe, err)
		return a.scheduleSubscribeRestart()
	}
	_ = a.MonitorNode(a.opts.Endpoint.Node)

	a.labels.Count(a, metricSubscribeAttempts, "ok")
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

// updateRejection names why a pushed update was not applied, empty when it was.
func (a *readerActor) updateRejection(from gen.PID, update SnapshotUpdate) string {
	switch {
	case !a.subscribed:
		return "unsubscribed"
	case from != a.controllerPID:
		return "wrong_sender"
	case update.Snapshot == nil:
		return "empty"
	case update.Snapshot.Generation <= a.lastGeneration:
		return "stale"
	default:
		return ""
	}
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
	_ = a.SendEvent(a.snapshotEvent.name, a.snapshotEvent.token, snap.Clone())
}

// reconcileStatus recomputes and, on change, sends the current reader status to the supervisor.
func (a *readerActor) reconcileStatus() {
	if !a.activated {
		return
	}
	next := a.status()
	if next == a.lastStatus {
		return
	}
	a.lastStatus = next
	_ = a.Send(a.Parent(), MessageReaderActorStatusChanged{status: next})
}

// status derives the reader's current publishable status, shared by reconcileStatus (to the
// supervisor) and HandleInspect (to an operator).
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

// HandleInspect exposes the subscription: whether it holds, why not, its generation, and to whom.
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

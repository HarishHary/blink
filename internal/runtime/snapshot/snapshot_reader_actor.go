package snapshot

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

var (
	ErrSnapshotRead      = errors.New("snapshot read failed")
	ErrSnapshotSubscribe = errors.New("snapshot event subscription failed")
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
	Err          error
}

// readerActor makes one bounded Call to subscribe and then receives pushed SnapshotUpdate messages;
// Ergo remote delivery is push-based, so there is no read loop or meta to supervise.
type readerActor struct {
	act.Actor
	opts            ReaderActorOptions
	snapshotEvent   eventPublication
	activated       bool
	controllerPID   gen.PID
	subscribed      bool
	lastGeneration  int64
	retry           *runtime.ScheduledBackoff
	lifecycle       ReaderActorLifecycle // the reader's own live lifecycle; the supervisor owns starting and restarting
	err             error                // the reader's own failure
	lastStatus      ReaderActorStatus    // last published projection, the baseline reconcileStatus dedupes against
	lastStatusEpoch int64
	labels          telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageReaderActorActivate permits subscription and carries the status epoch to continue after.
type MessageReaderActorActivate struct{ StatusEpoch int64 }

type MessageReaderActorStatusChanged struct {
	StatusEpoch int64
	Status      ReaderActorStatus
}

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

type MessageSubscribeRetry struct{ token uint64 }

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

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
	a.retry = runtime.NewScheduledBackoff(a.opts.RetryMin, a.opts.RetryMax)
	a.lifecycle = ReaderActorRunning
	return nil
}

// HandleMessage processes activation, pushed snapshot updates, controller-loss, and retry messages.
func (a *readerActor) HandleMessage(from gen.PID, message any) error {
	defer a.reconcileStatus()
	switch m := message.(type) {
	case MessageReaderActorActivate:
		if from != a.Parent() || a.activated {
			return nil
		}
		a.lastStatusEpoch = max(a.lastStatusEpoch, m.StatusEpoch)
		a.activated = true
		return a.subscribe()
	case SnapshotUpdate:
		if reason := a.updateRejection(from, m); reason != "" {
			a.labels.Count(a, metricUpdatesIgnored, reason)
			return nil
		}
		a.labels.Count(a, metricUpdates)
		a.lastGeneration = m.Snapshot.Generation
		a.publishSnapshot(m.Snapshot)
		a.err = nil
		a.reconcileStatus()
		return nil
	case MessageSubscribeRetry:
		if !a.retry.Pending || a.retry.Token != m.token || a.subscribed {
			return nil
		}
		a.retry.Pending = false
		a.retry.Cancel = nil
		return a.subscribe()
	case gen.MessageDownPID:
		if !a.subscribed || m.PID != a.controllerPID {
			return nil
		}
		a.labels.Count(a, metricControllerDown, "process")
		a.controllerPID = gen.PID{}
		a.subscribed = false
		a.err = m.Reason
		a.reconcileStatus()
		a.Log().Error("snapshot reader actor: controller %s stopped: %v", m.PID, m.Reason)
		return a.scheduleSubscribeRetry()
	case gen.MessageDownNode:
		if !a.subscribed || m.Name != a.opts.Endpoint.Node {
			return nil
		}
		a.labels.Count(a, metricControllerDown, "node")
		a.controllerPID = gen.PID{}
		a.subscribed = false
		a.err = fmt.Errorf("controller node %s down", m.Name)
		a.reconcileStatus()
		a.Log().Error("snapshot reader actor: controller node %s down", m.Name)
		return a.scheduleSubscribeRetry()
	}
	return nil
}

// HandleCall rejects unsupported synchronous requests.
func (a *readerActor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot reader actor: unsupported call %T", request), nil
}

// Terminate cancels any pending resubscribe and notifies the controller, best effort.
func (a *readerActor) Terminate(reason error) {
	a.lifecycle = ReaderActorStopped
	a.err = runtime.FirstError(reason, a.err)
	defer a.reconcileStatus()
	if a.retry != nil {
		a.retry.CancelScheduled(false)
	}
	if a.subscribed {
		_ = a.SendWithPriority(a.opts.Endpoint, UnsubscribeRequest{ExecutorID: a.opts.ExecutorID}, gen.MessagePriorityHigh)
	}
	a.controllerPID = gen.PID{}
	a.subscribed = false
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// subscribe issues a bounded Call to the controller, then monitors it for loss detection.
func (a *readerActor) subscribe() error {
	request := SubscribeRequest{
		ExecutorID:      a.opts.ExecutorID,
		KnownGeneration: a.lastGeneration,
	}
	response, err := a.CallProcessID(a.opts.Endpoint, request, subscribeTimeoutSeconds)
	if err != nil {
		a.labels.Count(a, metricSubscribeAttempts, "unreachable")
		a.err = fmt.Errorf("%w: subscribe: %w", ErrSnapshotSubscribe, err)
		a.reconcileStatus()
		return a.scheduleSubscribeRetry()
	}
	sub, ok := response.(SubscribeResponse)
	if !ok {
		a.labels.Count(a, metricSubscribeAttempts, "bad_response")
		a.err = fmt.Errorf("%w: subscribe: unexpected response %T", ErrSnapshotSubscribe, response)
		a.reconcileStatus()
		return a.scheduleSubscribeRetry()
	}
	if err := a.MonitorPID(sub.ControllerPID); err != nil {
		a.labels.Count(a, metricSubscribeAttempts, "unmonitorable")
		a.err = fmt.Errorf("%w: monitor controller: %w", ErrSnapshotSubscribe, err)
		a.reconcileStatus()
		return a.scheduleSubscribeRetry()
	}
	_ = a.MonitorNode(a.opts.Endpoint.Node)

	a.labels.Count(a, metricSubscribeAttempts, "ok")
	a.retry.CancelScheduled(true)
	a.controllerPID = sub.ControllerPID
	a.subscribed = true
	if sub.Current != nil && sub.Current.Generation > a.lastGeneration {
		a.lastGeneration = sub.Current.Generation
		a.publishSnapshot(sub.Current)
	}
	a.err = nil
	a.reconcileStatus()
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

// publishSnapshot publishes a cloned snapshot.
func (a *readerActor) publishSnapshot(snap *Snapshot) {
	//argus:allow A1001 cloned snapshot transfers to the event; subscribers treat the shared event as immutable
	_ = a.SendEvent(a.snapshotEvent.name, a.snapshotEvent.token, snap.Clone())
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// scheduleSubscribeRetry schedules a backoff-delayed resubscribe.
func (a *readerActor) scheduleSubscribeRetry() error {
	if a.retry.Pending {
		return nil
	}
	delay := a.retry.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("snapshot reader retry: %w", runtime.ErrBackoffStopped)
	}
	a.retry.Token++
	token := a.retry.Token
	cancel, err := a.SendWithPriorityAfter(a.PID(), MessageSubscribeRetry{token: token}, gen.MessagePriorityHigh, delay)
	if err != nil {
		return fmt.Errorf("schedule snapshot reader retry: %w", err)
	}
	a.retry.Pending = true
	a.retry.Cancel = cancel
	return nil
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// reconcileStatus recomputes and, on change, sends the current reader status to the supervisor.
func (a *readerActor) reconcileStatus() {
	if !a.activated {
		return
	}
	next := a.status()
	if sameReaderActorStatus(a.lastStatus, next) {
		return
	}
	a.lastStatusEpoch = runtime.NextStatusEpoch(a.lastStatusEpoch)
	a.lastStatus = next
	a.propagateStatus(next)
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (a *readerActor) propagateStatus(next ReaderActorStatus) {
	_ = a.SendWithPriority(a.Parent(), MessageReaderActorStatusChanged{StatusEpoch: a.lastStatusEpoch, Status: next}, gen.MessagePriorityHigh)
}

// status derives the reader's current publishable status, shared by reconcileStatus (to the
// supervisor) and HandleInspect (to an operator).
func (a *readerActor) status() ReaderActorStatus {
	availability := runtime.AvailabilityUnavailable
	if a.subscribed {
		availability = runtime.AvailabilityReady
	}
	return ReaderActorStatus{
		Lifecycle:    a.lifecycle,
		Availability: availability,
		Generation:   a.lastGeneration,
		Err:          a.err,
	}
}

// HandleInspect exposes the subscription: whether it holds, why not, its generation, and to whom.
func (a *readerActor) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	return map[string]string{
		"reader:err":          runtime.ErrorText(status.Err),
		"reader:lifecycle":    string(status.Lifecycle),
		"reader:availability": string(status.Availability),
		"reader:subscribed":   fmt.Sprintf("%t", a.subscribed),
		"reader:generation":   fmt.Sprintf("%d", a.lastGeneration),
		"reader:controller":   a.controllerPID.String(),
		"reader:executor_id":  a.opts.ExecutorID,
	}
}

// sameReaderActorStatus compares the status fields that trigger publication.
func sameReaderActorStatus(left, right ReaderActorStatus) bool {
	return left.Lifecycle == right.Lifecycle &&
		left.Availability == right.Availability &&
		left.Generation == right.Generation &&
		runtime.ErrorText(left.Err) == runtime.ErrorText(right.Err)
}

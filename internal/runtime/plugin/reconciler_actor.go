package plugin

import (
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
	snapshotruntime "github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/internal/snapshot"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// ReconcilerActorLifecycle describes the stable actor that projects
// control-plane snapshots and local artifacts into runtime desired state.
type ReconcilerActorLifecycle string

const (
	ReconcilerActorStarting   ReconcilerActorLifecycle = "starting"
	ReconcilerActorRunning    ReconcilerActorLifecycle = "running"
	ReconcilerActorRestarting ReconcilerActorLifecycle = "restarting"
	ReconcilerActorStopped    ReconcilerActorLifecycle = "stopped"
)

// reconcilerActorState tracks the desired-state reconciler actor.
type reconcilerActorState struct {
	pid    gen.PID
	status ReconcilerActorStatus
}

// ReconcilerActorStatus reports the reconciler state to its supervisor.
type ReconcilerActorStatus struct {
	Lifecycle          ReconcilerActorLifecycle
	Availability       runtime.Availability
	SnapshotGeneration int64
	Revision           uint64
}

// artifactResolverMetaState tracks the resolver meta-process state and restart policy.
type artifactResolverMetaState struct {
	alias   gen.Alias
	restart *runtime.ScheduledBackoff
	status  artifactResolverMetaStatus
}

// artifactWatcherMetaState tracks the watcher meta-process state and restart policy.
type artifactWatcherMetaState struct {
	alias   gen.Alias
	restart *runtime.ScheduledBackoff
	status  artifactWatcherMetaStatus
}

// reconcilerActor subscribes to the buffered snapshot event and is
// the stable owner of the current snapshot, desired-state revisions, resolution
// coalescing, and all retry policies. It independently replaces its resolver and
// watcher meta-processes so an expected I/O-process failure does not discard the
// actor's state or event subscription.
type reconcilerActor struct {
	act.Actor
	directory        string
	revision         uint64
	events           snapshotruntime.Events
	snapshot         *snapshot.Snapshot
	readerActorReady bool
	resolutionRetry  *runtime.ScheduledBackoff
	resolver         artifactResolverMetaState
	watcher          artifactWatcherMetaState
	resolving        bool
	dirty            bool
	deferred         bool
}

// ---- messages ----

// MessageReconcilerActorActivate gives a replacement reconciler a
// revision base newer than the last state accepted by its supervisor.
type MessageReconcilerActorActivate struct{ revisionBase uint64 }

// MessageReconcilerActorStatusChanged publishes the reconciler status to its supervisor.
type MessageReconcilerActorStatusChanged struct{ status ReconcilerActorStatus }

// MessageResolutionRetry retries deferred artifact resolution after backoff.
type MessageResolutionRetry struct{ token uint64 }

// MessageArtifactResolverMetaRestart restarts the artifact resolver meta-process after backoff.
type MessageArtifactResolverMetaRestart struct{ token uint64 }

// MessageArtifactWatcherMetaRestart restarts the artifact watcher meta-process after backoff.
type MessageArtifactWatcherMetaRestart struct{ token uint64 }

// MessageDesiredStateFreshness challenges and confirms that the exact generation and revision remain current.
type MessageDesiredStateFreshness struct {
	snapshotGeneration int64
	desiredRevision    uint64
}

// ---------------------------------------------------------------------------
// Constructor & actor lifecycle
// ---------------------------------------------------------------------------

// newReconcilerActor constructs a reconciler actor with independent retry policies.
func newReconcilerActor(events snapshotruntime.Events, directory string, retryMin, retryMax time.Duration) gen.ProcessBehavior {
	return &reconcilerActor{
		events:          events,
		directory:       directory,
		resolutionRetry: runtime.NewScheduledBackoff(retryMin, retryMax),
		resolver: artifactResolverMetaState{
			restart: runtime.NewScheduledBackoff(retryMin, retryMax),
			status: artifactResolverMetaStatus{
				Lifecycle:    ArtifactResolverMetaStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
		},
		watcher: artifactWatcherMetaState{
			restart: runtime.NewScheduledBackoff(retryMin, retryMax),
			status: artifactWatcherMetaStatus{
				Lifecycle:    ArtifactWatcherMetaStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
		},
	}
}

// Init validates the reconciler actor's required events.
func (a *reconcilerActor) Init(...any) error {
	if a.events.Snapshot.Name == "" || a.events.Status.Name == "" {
		return fmt.Errorf("plugin reconciler: snapshot events are required")
	}
	return nil
}

// Terminate stops retries and shuts down the artifact meta-processes.
func (a *reconcilerActor) Terminate(error) {
	a.resolutionRetry.CancelScheduled(false)
	a.resolver.restart.CancelScheduled(false)
	a.watcher.restart.CancelScheduled(false)
	a.stopArtifactResolverMeta(gen.TerminateReasonShutdown)
	a.stopArtifactWatcherMeta(gen.TerminateReasonShutdown)
}

// ---------------------------------------------------------------------------
// Message ingress
// ---------------------------------------------------------------------------

// HandleMessage processes reconciler control messages and meta-process lifecycle events.
func (a *reconcilerActor) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageReconcilerActorActivate:
		if from != a.Parent() {
			return nil
		}

		a.revision = m.revisionBase
		a.publishStatus()

		for _, event := range []gen.Event{a.events.Snapshot, a.events.Status} {
			buffered, err := a.MonitorEvent(event)
			if err != nil {
				return fmt.Errorf("monitor reconciler event %q: %w", event.Name, err)
			}
			for _, bufferedEvent := range buffered {
				if err := a.applyEvent(bufferedEvent); err != nil {
					return err
				}
			}
		}

		if err := a.startArtifactResolverMeta(); err != nil {
			return err
		}
		return a.startArtifactWatcherMeta()

	case MessageDesiredStateFreshness:
		if from != a.Parent() {
			return nil
		}
		if a.snapshot == nil ||
			m.snapshotGeneration != a.snapshot.Generation ||
			m.desiredRevision != a.revision ||
			a.dirty || a.resolving || a.deferred ||
			!a.readerActorReady ||
			a.resolver.status.Availability != runtime.AvailabilityReady ||
			a.watcher.status.Availability != runtime.AvailabilityReady {
			return nil
		}
		return a.Send(a.Parent(), MessageDesiredStateFreshness{
			snapshotGeneration: m.snapshotGeneration,
			desiredRevision:    m.desiredRevision,
		})

	case MessageArtifactWatcherStateChanged:
		if from != a.PID() || m.source != a.watcher.alias || a.watcher.alias == (gen.Alias{}) {
			return nil
		}
		started := a.watcher.status.Lifecycle != ArtifactWatcherMetaRunning
		a.watcher.status.Lifecycle = ArtifactWatcherMetaRunning
		if m.directoryReadable && m.watchingDirectory {
			a.watcher.status.Availability = runtime.AvailabilityReady
		} else {
			a.watcher.status.Availability = runtime.AvailabilityDegraded
		}
		if started {
			a.watcher.restart.CancelScheduled(true)
			if a.snapshot != nil {
				// The directory may have changed while no watcher instance was alive.
				a.dirty = true
				a.resolutionRetry.CancelScheduled(false)
			}
		}
		a.publishStatus()
		if started {
			return a.requestResolve()
		}

	case MessageArtifactResolutionResult:
		if from != a.PID() || m.source != a.resolver.alias || a.resolver.alias == (gen.Alias{}) || !a.resolving {
			return nil
		}
		started := a.resolver.status.Lifecycle != ArtifactResolverMetaRunning
		a.resolving = false
		a.resolver.status.Lifecycle = ArtifactResolverMetaRunning
		a.resolver.status.Availability = runtime.AvailabilityReady
		if started {
			a.resolver.restart.CancelScheduled(true)
		}

		if a.snapshot == nil || m.snapshotGeneration != a.snapshot.Generation {
			a.dirty = true
			return a.requestResolve()
		}

		// A filesystem or snapshot change arrived while this resolution was in
		// flight. Discard the potentially stale result and resolve again from the
		// current snapshot and current filesystem state.
		if a.dirty {
			return a.requestResolve()
		}
		a.deferred = m.deferred
		if m.deferred {
			a.publishStatus()
			return a.scheduleResolutionRetry()
		}

		a.revision++
		if err := a.Send(a.Parent(), MessageProposeDesiredState{
			desired: MessageApplyCatalogDesiredState{
				desiredRevision:    a.revision,
				snapshotGeneration: a.snapshot.Generation,
				desired:            m.desired,
			},
		}); err != nil {
			return fmt.Errorf("propose resolved desired state: %w", err)
		}
		a.publishStatus()
		a.resolutionRetry.CancelScheduled(true)

	case MessageArtifactDirectoryChanged:
		if from != a.PID() ||
			m.source != a.watcher.alias ||
			a.watcher.alias == (gen.Alias{}) ||
			a.snapshot == nil {
			return nil
		}
		a.dirty = true
		// A concrete filesystem change is a useful immediate retry signal. Cancel
		// the delayed timer but preserve the current backoff progression until a
		// complete successful resolution resets it.
		a.resolutionRetry.CancelScheduled(false)
		return a.requestResolve()

	case MessageResolutionRetry:
		if !a.resolutionRetry.Pending || a.resolutionRetry.Token != m.token {
			return nil
		}
		a.resolutionRetry.Pending = false
		a.resolutionRetry.Cancel = nil
		a.dirty = true
		return a.requestResolve()

	case MessageArtifactResolverMetaRestart:
		if !a.resolver.restart.Pending ||
			a.resolver.restart.Token != m.token ||
			a.resolver.alias != (gen.Alias{}) {
			return nil
		}
		a.resolver.restart.Pending = false
		a.resolver.restart.Cancel = nil
		return a.startArtifactResolverMeta()

	case MessageArtifactWatcherMetaRestart:
		if !a.watcher.restart.Pending ||
			a.watcher.restart.Token != m.token ||
			a.watcher.alias != (gen.Alias{}) {
			return nil
		}
		a.watcher.restart.Pending = false
		a.watcher.restart.Cancel = nil
		return a.startArtifactWatcherMeta()

	case gen.MessageDownAlias:
		switch m.Alias {
		case a.resolver.alias:
			a.resolver.alias = gen.Alias{}
			a.resolver.status.Lifecycle = ArtifactResolverMetaRestarting
			a.resolver.status.Availability = runtime.AvailabilityUnavailable
			a.resolving = false
			a.dirty = a.snapshot != nil
			a.resolutionRetry.CancelScheduled(false)
			a.publishStatus()
			return a.scheduleResolverRestart()

		case a.watcher.alias:
			a.watcher.alias = gen.Alias{}
			a.watcher.status.Lifecycle = ArtifactWatcherMetaRestarting
			a.watcher.status.Availability = runtime.AvailabilityUnavailable
			a.publishStatus()
			return a.scheduleWatcherRestart()
		}

	case gen.MessageDownEvent:
		if m.Event == a.events.Snapshot || m.Event == a.events.Status {
			return fmt.Errorf("snapshot event terminated: %w", m.Reason)
		}
	}
	return nil
}

// HandleEvent applies a monitored snapshot or reader-status event.
func (a *reconcilerActor) HandleEvent(event gen.MessageEvent) error {
	return a.applyEvent(event)
}

// HandleCall rejects synchronous calls because the reconciler has no call API.
func (a *reconcilerActor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported desired-state reconciler call %T", request), nil
}

// ---------------------------------------------------------------------------
// Artifact meta-process lifecycle
// ---------------------------------------------------------------------------

// startArtifactResolverMeta starts and monitors the artifact resolver meta-process.
func (a *reconcilerActor) startArtifactResolverMeta() error {
	if a.resolver.alias != (gen.Alias{}) {
		return nil
	}

	a.resolver.status.Lifecycle = ArtifactResolverMetaStarting
	a.resolver.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactResolverMeta{
			directory: a.directory,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.resolver.status.Lifecycle = ArtifactResolverMetaRestarting
		a.Log().Error("artifact resolver meta spawn failed: error=%v", err)
		a.publishStatus()
		if retryErr := a.scheduleResolverRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact resolver meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.resolver.status.Lifecycle = ArtifactResolverMetaRestarting
		a.Log().Error("artifact resolver meta monitor failed: error=%v", err)
		a.publishStatus()
		if retryErr := a.scheduleResolverRestart(); retryErr != nil {
			return fmt.Errorf("monitor artifact resolver meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	a.resolver.alias = alias
	return a.requestResolve()
}

// startArtifactWatcherMeta starts and monitors the artifact watcher meta-process.
func (a *reconcilerActor) startArtifactWatcherMeta() error {
	if a.watcher.alias != (gen.Alias{}) {
		return nil
	}

	a.watcher.status.Lifecycle = ArtifactWatcherMetaStarting
	a.watcher.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactWatcherMeta{
			directory: a.directory,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.watcher.status.Lifecycle = ArtifactWatcherMetaRestarting
		a.Log().Error("artifact watcher meta spawn failed: error=%v", err)
		a.publishStatus()
		if retryErr := a.scheduleWatcherRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact watcher meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.watcher.status.Lifecycle = ArtifactWatcherMetaRestarting
		a.Log().Error("artifact watcher meta monitor failed: error=%v", err)
		a.publishStatus()
		if retryErr := a.scheduleWatcherRestart(); retryErr != nil {
			return fmt.Errorf("monitor artifact watcher meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	a.watcher.alias = alias
	return nil
}

// stopArtifactResolverMeta stops and demonitors the artifact resolver meta-process.
func (a *reconcilerActor) stopArtifactResolverMeta(reason error) {
	if a.resolver.alias == (gen.Alias{}) {
		return
	}
	alias := a.resolver.alias
	a.resolver.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

// stopArtifactWatcherMeta stops and demonitors the artifact watcher meta-process.
func (a *reconcilerActor) stopArtifactWatcherMeta(reason error) {
	if a.watcher.alias == (gen.Alias{}) {
		return
	}
	alias := a.watcher.alias
	a.watcher.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

// ---------------------------------------------------------------------------
// Snapshot & resolution reconciliation
// ---------------------------------------------------------------------------

// applyEvent incorporates monitored snapshot and reader-status events.
func (a *reconcilerActor) applyEvent(event gen.MessageEvent) error {
	switch event.Event {
	case a.events.Status:
		status, ok := event.Message.(snapshotruntime.ReaderActorStatus)
		if !ok {
			return nil
		}
		a.readerActorReady = status.Availability == runtime.AvailabilityReady
		a.publishStatus()
		return nil

	case a.events.Snapshot:
		snap, ok := event.Message.(*snapshot.Snapshot)
		if !ok || snap == nil {
			return nil
		}
		if a.snapshot != nil && snap.Generation < a.snapshot.Generation {
			return nil
		}
		if a.snapshot == nil || snap.Generation > a.snapshot.Generation {
			// A new desired generation should not inherit the retry penalty of the
			// previous generation.
			a.resolutionRetry.CancelScheduled(true)
		} else {
			a.resolutionRetry.CancelScheduled(false)
		}

		a.snapshot = snap.Clone()
		a.dirty = true
		a.deferred = false
		a.publishStatus()
		return a.requestResolve()

	default:
		return nil
	}
}

// requestResolve starts artifact resolution when current state requires it.
func (a *reconcilerActor) requestResolve() error {
	if a.resolving || !a.dirty || a.snapshot == nil || a.resolver.alias == (gen.Alias{}) {
		return nil
	}
	a.resolving = true
	a.dirty = false
	a.publishStatus()
	snap := a.snapshot.Clone()
	if err := a.Send(a.resolver.alias, MessageResolveArtifacts{snapshot: *snap}); err != nil {
		a.resolving = false
		a.dirty = true
		a.resolver.status.Availability = runtime.AvailabilityDegraded
		a.publishStatus()
		if retryErr := a.scheduleResolutionRetry(); retryErr != nil {
			return fmt.Errorf("send artifact resolve request: %v; schedule retry: %w", err, retryErr)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Retry scheduling
// ---------------------------------------------------------------------------

// scheduleResolutionRetry schedules a delayed artifact-resolution retry.
func (a *reconcilerActor) scheduleResolutionRetry() error {
	if a.resolutionRetry.Pending {
		return nil
	}

	delay := a.resolutionRetry.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("desired-state resolution retry: %w", runtime.ErrBackoffStopped)
	}

	a.resolutionRetry.Token++
	token := a.resolutionRetry.Token
	cancel, err := a.SendAfter(a.PID(), MessageResolutionRetry{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule desired-state resolution retry: %w", err)
	}
	a.resolutionRetry.Pending = true
	a.resolutionRetry.Cancel = cancel
	return nil
}

// scheduleResolverRestart schedules a delayed resolver meta-process restart.
func (a *reconcilerActor) scheduleResolverRestart() error {
	if a.resolver.restart.Pending {
		return nil
	}

	delay := a.resolver.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact resolver restart: %w", runtime.ErrBackoffStopped)
	}

	a.resolver.restart.Token++
	token := a.resolver.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageArtifactResolverMetaRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact resolver restart: %w", err)
	}
	a.resolver.restart.Pending = true
	a.resolver.restart.Cancel = cancel
	a.resolver.status.Lifecycle = ArtifactResolverMetaRestarting
	a.resolver.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

// scheduleWatcherRestart schedules a delayed watcher meta-process restart.
func (a *reconcilerActor) scheduleWatcherRestart() error {
	if a.watcher.restart.Pending {
		return nil
	}

	delay := a.watcher.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact watcher restart: %w", runtime.ErrBackoffStopped)
	}

	a.watcher.restart.Token++
	token := a.watcher.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageArtifactWatcherMetaRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact watcher restart: %w", err)
	}
	a.watcher.restart.Pending = true
	a.watcher.restart.Cancel = cancel
	a.watcher.status.Lifecycle = ArtifactWatcherMetaRestarting
	a.watcher.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

// ---------------------------------------------------------------------------
// Status projection
// ---------------------------------------------------------------------------

// publishStatus reports the current reconciler availability to its supervisor.
func (a *reconcilerActor) publishStatus() {
	availability := runtime.AvailabilityReady
	switch {
	case !a.readerActorReady ||
		a.snapshot == nil ||
		a.dirty ||
		a.resolving ||
		a.deferred ||
		a.resolver.status.Availability == runtime.AvailabilityUnavailable:
		availability = runtime.AvailabilityUnavailable
	case a.watcher.status.Availability != runtime.AvailabilityReady:
		availability = runtime.AvailabilityDegraded
	}

	var snapshotGeneration int64
	if a.snapshot != nil {
		snapshotGeneration = a.snapshot.Generation
	}

	_ = a.Send(a.Parent(), MessageReconcilerActorStatusChanged{status: ReconcilerActorStatus{
		Lifecycle:          ReconcilerActorRunning,
		Availability:       availability,
		SnapshotGeneration: snapshotGeneration,
		Revision:           a.revision,
	}})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// errorText returns an empty string for nil errors and the error text otherwise.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

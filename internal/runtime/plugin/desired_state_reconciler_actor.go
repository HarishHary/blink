package plugin

import (
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// desiredStateReconcilerActivate is sent by runtimeSupervisor after the child
// reaches Running state. It fences actor incarnations and gives a replacement
// reconciler a revision base that is newer than the last state accepted by the
// supervisor.
type desiredStateReconcilerActivate struct {
	generation   uint64
	revisionBase uint64
}

type desiredStateResolutionRetry struct{ token uint64 }
type artifactResolverRestart struct{ token uint64 }
type artifactWatcherRestart struct{ token uint64 }

type artifactResolve struct {
	resolverGeneration uint64
	id                 uint64
	snapshotGeneration int64
	snapshot           *snapshot.Snapshot
}

type artifactResolved struct {
	resolverGeneration uint64
	id                 uint64
	snapshotGeneration int64
	desired            map[string]routerApplyDesired
	deferred           bool
}

type artifactResolverStarted struct{ generation uint64 }
type artifactWatcherStarted struct{ generation uint64 }

type artifactWatcherStateChanged struct {
	generation        uint64
	directoryReadable bool
	watchingDirectory bool
	err               string
}

// DesiredStateReconcilerLifecycle describes the stable actor that projects
// control-plane snapshots and local artifacts into runtime desired state.
type DesiredStateReconcilerLifecycle string

const (
	DesiredStateReconcilerStarting   DesiredStateReconcilerLifecycle = "starting"
	DesiredStateReconcilerRunning    DesiredStateReconcilerLifecycle = "running"
	DesiredStateReconcilerRestarting DesiredStateReconcilerLifecycle = "restarting"
	DesiredStateReconcilerStopped    DesiredStateReconcilerLifecycle = "stopped"
)

// ArtifactResolverLifecycle describes one resolver meta-process incarnation.
type ArtifactResolverLifecycle string

const (
	ArtifactResolverStarting   ArtifactResolverLifecycle = "starting"
	ArtifactResolverRunning    ArtifactResolverLifecycle = "running"
	ArtifactResolverRestarting ArtifactResolverLifecycle = "restarting"
	ArtifactResolverStopped    ArtifactResolverLifecycle = "stopped"
)

// ArtifactResolverStatus is owned by desiredStateReconcilerActor because the
// actor owns resolver generations, restart policy, and alias monitoring.
type ArtifactResolverStatus struct {
	Lifecycle      ArtifactResolverLifecycle
	Availability   runtime.Availability
	Generation     uint64
	RestartPending bool
	LastError      string
}

// ArtifactWatcherLifecycle describes one watcher meta-process incarnation.
type ArtifactWatcherLifecycle string

const (
	ArtifactWatcherStarting   ArtifactWatcherLifecycle = "starting"
	ArtifactWatcherRunning    ArtifactWatcherLifecycle = "running"
	ArtifactWatcherRestarting ArtifactWatcherLifecycle = "restarting"
	ArtifactWatcherStopped    ArtifactWatcherLifecycle = "stopped"
)

// ArtifactWatcherStatus is owned by desiredStateReconcilerActor. The watcher
// meta-process reports directory facts; the actor derives lifecycle and
// availability and owns restart state.
type ArtifactWatcherStatus struct {
	Lifecycle         ArtifactWatcherLifecycle
	Availability      runtime.Availability
	Generation        uint64
	RestartPending    bool
	DirectoryReadable bool
	WatchingDirectory bool
	LastError         string
}

// DesiredStateReconcilerStatus groups the independently managed resolver and
// watcher statuses instead of flattening their fields into the parent.
type DesiredStateReconcilerStatus struct {
	Lifecycle          DesiredStateReconcilerLifecycle
	Availability       runtime.Availability
	ActorGeneration    uint64
	ActorLastError     string
	SnapshotGeneration int64
	Revision           uint64
	Resolving          bool
	Deferred           bool
	Resolver           ArtifactResolverStatus
	Watcher            ArtifactWatcherStatus
}

type desiredStateReconcilerStatusChanged struct {
	generation uint64
	status     DesiredStateReconcilerStatus
}

// artifactDirectoryChanged is emitted by artifactWatcherMeta. The watcher
// generation fences notifications from replaced watcher incarnations.
type artifactDirectoryChanged struct{ watcherGeneration uint64 }

// desiredStateReconcilerActor subscribes to the buffered snapshot event and is
// the stable owner of the current snapshot, desired-state revisions, resolution
// coalescing, and all retry policies. It independently replaces its resolver and
// watcher meta-processes so an expected I/O-process failure does not discard the
// actor's state or event subscription.
type desiredStateReconcilerActor[T plugin.Syncable] struct {
	act.Actor

	actorGeneration uint64
	activated       bool
	revision        uint64

	snapshotEvent gen.Event
	directory     string
	adapter       *plugin.PluginAdapter[T]

	resolutionRetry scheduledBackoff
	resolverRestart scheduledBackoff
	watcherRestart  scheduledBackoff

	resolverAlias  gen.Alias
	resolverStatus ArtifactResolverStatus

	watcherAlias  gen.Alias
	watcherStatus ArtifactWatcherStatus

	current   *snapshot.Snapshot
	resolveID uint64
	resolving bool
	dirty     bool
	deferred  bool
}

func newDesiredStateReconcilerActor[T plugin.Syncable](
	snapshotEvent gen.Event,
	directory string,
	adapter *plugin.PluginAdapter[T],
	retryMin,
	retryMax time.Duration,
) gen.ProcessBehavior {
	return &desiredStateReconcilerActor[T]{
		snapshotEvent: snapshotEvent,
		directory:     directory,
		adapter:       adapter,
		resolutionRetry: scheduledBackoff{
			strategy: newDesiredStateBackoff(retryMin, retryMax),
		},
		resolverRestart: scheduledBackoff{
			strategy: newDesiredStateBackoff(retryMin, retryMax),
		},
		watcherRestart: scheduledBackoff{
			strategy: newDesiredStateBackoff(retryMin, retryMax),
		},
		resolverStatus: ArtifactResolverStatus{
			Lifecycle:    ArtifactResolverStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
		watcherStatus: ArtifactWatcherStatus{
			Lifecycle:    ArtifactWatcherStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
}

func newDesiredStateBackoff(minDelay, maxDelay time.Duration) *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(minDelay),
		backoff.WithMaxInterval(maxDelay),
		backoff.WithMultiplier(2),
		backoff.WithMaxElapsedTime(0),
	)
}

func (a *desiredStateReconcilerActor[T]) Init(...any) error { return nil }

func (a *desiredStateReconcilerActor[T]) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case desiredStateReconcilerActivate:
		if m.generation <= a.actorGeneration {
			return nil
		}
		if a.activated {
			return fmt.Errorf(
				"desired-state reconciler already activated as generation %d",
				a.actorGeneration,
			)
		}

		a.actorGeneration = m.generation
		a.revision = m.revisionBase
		a.activated = true
		a.publishStatus()

		buffered, err := a.MonitorEvent(a.snapshotEvent)
		if err != nil {
			return fmt.Errorf("monitor snapshot event %s: %w", a.snapshotEvent, err)
		}
		for _, event := range buffered {
			if err := a.acceptEvent(event); err != nil {
				return err
			}
		}

		if err := a.startArtifactResolverMeta(); err != nil {
			return err
		}
		return a.startArtifactWatcherMeta()

	case artifactResolverStarted:
		if m.generation != a.resolverStatus.Generation || a.resolverAlias == (gen.Alias{}) {
			return nil
		}
		a.resolverStatus.Lifecycle = ArtifactResolverRunning
		a.resolverStatus.Availability = runtime.AvailabilityReady
		a.resolverStatus.LastError = ""
		a.resetResolverRestartBackoff()
		a.publishStatus()
		return a.requestResolve()

	case artifactWatcherStarted:
		if m.generation != a.watcherStatus.Generation || a.watcherAlias == (gen.Alias{}) {
			return nil
		}
		a.watcherStatus.Lifecycle = ArtifactWatcherRunning
		a.watcherStatus.Availability = runtime.AvailabilityDegraded
		a.watcherStatus.LastError = ""
		a.resetWatcherRestartBackoff()
		a.publishStatus()
		if a.current != nil {
			// The directory may have changed while no watcher incarnation was alive.
			a.dirty = true
			a.cancelDesiredStateResolutionRetry(false)
		}
		return a.requestResolve()

	case artifactWatcherStateChanged:
		if m.generation != a.watcherStatus.Generation || a.watcherAlias == (gen.Alias{}) {
			return nil
		}
		a.watcherStatus.Lifecycle = ArtifactWatcherRunning
		a.watcherStatus.DirectoryReadable = m.directoryReadable
		a.watcherStatus.WatchingDirectory = m.watchingDirectory
		a.watcherStatus.LastError = m.err
		if m.directoryReadable && m.watchingDirectory {
			a.watcherStatus.Availability = runtime.AvailabilityReady
		} else {
			a.watcherStatus.Availability = runtime.AvailabilityDegraded
		}
		a.publishStatus()

	case artifactResolved:
		if m.resolverGeneration != a.resolverStatus.Generation ||
			!a.resolving ||
			m.id != a.resolveID {
			return nil
		}
		a.resolving = false

		if a.current == nil || m.snapshotGeneration != a.current.Generation {
			a.dirty = true
			return a.requestResolve()
		}

		// A filesystem or snapshot change arrived while this resolution was in
		// flight. Discard the potentially stale result and resolve again from the
		// current snapshot and current filesystem state.
		if a.dirty {
			return a.requestResolve()
		}

		a.revision++
		a.deferred = m.deferred
		if err := a.Send(a.Parent(), runtimeApplySnapshot{
			generation: a.actorGeneration,
			snapshot: catalogApplySnapshot{
				revision: a.revision,
				desired:  m.desired,
			},
		}); err != nil {
			return fmt.Errorf("publish resolved desired state: %w", err)
		}
		a.publishStatus()

		if m.deferred {
			return a.scheduleDesiredStateResolutionRetry()
		}
		a.resetDesiredStateResolutionBackoff()

	case artifactDirectoryChanged:
		if m.watcherGeneration != a.watcherStatus.Generation ||
			a.watcherAlias == (gen.Alias{}) ||
			a.current == nil {
			return nil
		}
		a.dirty = true
		// A concrete filesystem change is a useful immediate retry signal. Cancel
		// the delayed timer but preserve the current backoff progression until a
		// complete successful resolution resets it.
		a.cancelDesiredStateResolutionRetry(false)
		return a.requestResolve()

	case desiredStateResolutionRetry:
		if !a.resolutionRetry.pending || a.resolutionRetry.token != m.token {
			return nil
		}
		a.resolutionRetry.pending = false
		a.resolutionRetry.cancel = nil
		a.dirty = true
		return a.requestResolve()

	case artifactResolverRestart:
		if !a.resolverRestart.pending ||
			a.resolverRestart.token != m.token ||
			a.resolverAlias != (gen.Alias{}) {
			return nil
		}
		a.resolverRestart.pending = false
		a.resolverRestart.cancel = nil
		return a.startArtifactResolverMeta()

	case artifactWatcherRestart:
		if !a.watcherRestart.pending ||
			a.watcherRestart.token != m.token ||
			a.watcherAlias != (gen.Alias{}) {
			return nil
		}
		a.watcherRestart.pending = false
		a.watcherRestart.cancel = nil
		return a.startArtifactWatcherMeta()

	case gen.MessageDownAlias:
		switch m.Alias {
		case a.resolverAlias:
			a.resolverAlias = gen.Alias{}
			a.resolverStatus.Lifecycle = ArtifactResolverRestarting
			a.resolverStatus.Availability = runtime.AvailabilityUnavailable
			a.resolverStatus.LastError = errorText(m.Reason)
			a.resolving = false
			a.resolveID++
			a.dirty = a.current != nil
			a.cancelDesiredStateResolutionRetry(false)
			a.publishStatus()
			return a.scheduleResolverRestart()

		case a.watcherAlias:
			a.watcherAlias = gen.Alias{}
			a.watcherStatus.Lifecycle = ArtifactWatcherRestarting
			a.watcherStatus.Availability = runtime.AvailabilityUnavailable
			a.watcherStatus.DirectoryReadable = false
			a.watcherStatus.WatchingDirectory = false
			a.watcherStatus.LastError = errorText(m.Reason)
			a.publishStatus()
			return a.scheduleWatcherRestart()
		}

	case gen.MessageDownEvent:
		if m.Event == a.snapshotEvent {
			return fmt.Errorf("snapshot event terminated: %w", m.Reason)
		}
	}
	return nil
}

func (a *desiredStateReconcilerActor[T]) HandleEvent(event gen.MessageEvent) error {
	return a.acceptEvent(event)
}

func (a *desiredStateReconcilerActor[T]) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (a *desiredStateReconcilerActor[T]) Terminate(error) {
	a.cancelDesiredStateResolutionRetry(false)
	a.cancelResolverRestart(false)
	a.cancelWatcherRestart(false)
	a.stopArtifactResolverMeta(gen.TerminateReasonShutdown)
	a.stopArtifactWatcherMeta(gen.TerminateReasonShutdown)
	a.resolverStatus.Lifecycle = ArtifactResolverStopped
	a.resolverStatus.Availability = runtime.AvailabilityUnavailable
	a.watcherStatus.Lifecycle = ArtifactWatcherStopped
	a.watcherStatus.Availability = runtime.AvailabilityUnavailable
}

func (a *desiredStateReconcilerActor[T]) startArtifactResolverMeta() error {
	if a.resolverAlias != (gen.Alias{}) {
		return nil
	}

	a.resolverStatus.Generation++
	a.resolverStatus.Lifecycle = ArtifactResolverStarting
	a.resolverStatus.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactResolverMeta[T]{
			directory:  a.directory,
			adapter:    a.adapter,
			generation: a.resolverStatus.Generation,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.resolverStatus.Lifecycle = ArtifactResolverRestarting
		a.resolverStatus.LastError = fmt.Sprintf("spawn artifact resolver meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleResolverRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact resolver meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.resolverStatus.Lifecycle = ArtifactResolverRestarting
		a.resolverStatus.LastError = fmt.Sprintf("monitor artifact resolver meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleResolverRestart(); retryErr != nil {
			return fmt.Errorf("monitor artifact resolver meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	a.resolverAlias = alias
	return nil
}

func (a *desiredStateReconcilerActor[T]) startArtifactWatcherMeta() error {
	if a.watcherAlias != (gen.Alias{}) {
		return nil
	}

	a.watcherStatus.Generation++
	a.watcherStatus.Lifecycle = ArtifactWatcherStarting
	a.watcherStatus.Availability = runtime.AvailabilityUnavailable
	a.watcherStatus.DirectoryReadable = false
	a.watcherStatus.WatchingDirectory = false
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactWatcherMeta{
			directory:  a.directory,
			generation: a.watcherStatus.Generation,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.watcherStatus.Lifecycle = ArtifactWatcherRestarting
		a.watcherStatus.LastError = fmt.Sprintf("spawn artifact watcher meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleWatcherRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact watcher meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.watcherStatus.Lifecycle = ArtifactWatcherRestarting
		a.watcherStatus.LastError = fmt.Sprintf("monitor artifact watcher meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleWatcherRestart(); retryErr != nil {
			return fmt.Errorf("monitor artifact watcher meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	a.watcherAlias = alias
	return nil
}

func (a *desiredStateReconcilerActor[T]) stopArtifactResolverMeta(reason error) {
	if a.resolverAlias == (gen.Alias{}) {
		return
	}
	alias := a.resolverAlias
	a.resolverAlias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

func (a *desiredStateReconcilerActor[T]) stopArtifactWatcherMeta(reason error) {
	if a.watcherAlias == (gen.Alias{}) {
		return
	}
	alias := a.watcherAlias
	a.watcherAlias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

func (a *desiredStateReconcilerActor[T]) acceptEvent(event gen.MessageEvent) error {
	if event.Event != a.snapshotEvent {
		return nil
	}
	snap, ok := event.Message.(*snapshot.Snapshot)
	if !ok || snap == nil {
		return nil
	}
	if a.current != nil && snap.Generation < a.current.Generation {
		return nil
	}

	if a.current == nil || snap.Generation > a.current.Generation {
		// A new desired generation should not inherit the retry penalty of the
		// previous generation.
		a.resetDesiredStateResolutionBackoff()
	} else {
		a.cancelDesiredStateResolutionRetry(false)
	}

	a.current = snap
	a.dirty = true
	a.deferred = false
	a.publishStatus()
	return a.requestResolve()
}

func (a *desiredStateReconcilerActor[T]) requestResolve() error {
	if a.resolving ||
		!a.dirty ||
		a.current == nil ||
		a.resolverAlias == (gen.Alias{}) {
		return nil
	}

	a.resolving = true
	a.dirty = false
	a.resolveID++
	a.publishStatus()
	if err := a.Send(a.resolverAlias, artifactResolve{
		resolverGeneration: a.resolverStatus.Generation,
		id:                 a.resolveID,
		snapshotGeneration: a.current.Generation,
		snapshot:           a.current,
	}); err != nil {
		a.resolving = false
		a.dirty = true
		a.publishStatus()
		if retryErr := a.scheduleDesiredStateResolutionRetry(); retryErr != nil {
			return fmt.Errorf("send artifact resolve request: %v; schedule retry: %w", err, retryErr)
		}
	}
	return nil
}

func (a *desiredStateReconcilerActor[T]) scheduleDesiredStateResolutionRetry() error {
	if a.resolutionRetry.pending {
		return nil
	}

	delay := a.resolutionRetry.strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("desired-state resolution retry backoff stopped")
	}

	a.resolutionRetry.token++
	token := a.resolutionRetry.token
	cancel, err := a.SendAfter(a.PID(), desiredStateResolutionRetry{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule desired-state resolution retry: %w", err)
	}
	a.resolutionRetry.pending = true
	a.resolutionRetry.cancel = cancel
	return nil
}

func (a *desiredStateReconcilerActor[T]) scheduleResolverRestart() error {
	if a.resolverRestart.pending {
		return nil
	}

	delay := a.resolverRestart.strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact resolver restart backoff stopped")
	}

	a.resolverRestart.token++
	token := a.resolverRestart.token
	cancel, err := a.SendAfter(a.PID(), artifactResolverRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact resolver restart: %w", err)
	}
	a.resolverRestart.pending = true
	a.resolverRestart.cancel = cancel
	a.resolverStatus.Lifecycle = ArtifactResolverRestarting
	a.resolverStatus.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

func (a *desiredStateReconcilerActor[T]) scheduleWatcherRestart() error {
	if a.watcherRestart.pending {
		return nil
	}

	delay := a.watcherRestart.strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact watcher restart backoff stopped")
	}

	a.watcherRestart.token++
	token := a.watcherRestart.token
	cancel, err := a.SendAfter(a.PID(), artifactWatcherRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact watcher restart: %w", err)
	}
	a.watcherRestart.pending = true
	a.watcherRestart.cancel = cancel
	a.watcherStatus.Lifecycle = ArtifactWatcherRestarting
	a.watcherStatus.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

func (a *desiredStateReconcilerActor[T]) cancelScheduledBackoff(state *scheduledBackoff, reset bool) {
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
	state.pending = false
	state.token++
	if reset {
		state.strategy.Reset()
	}
}

func (a *desiredStateReconcilerActor[T]) cancelDesiredStateResolutionRetry(reset bool) {
	a.cancelScheduledBackoff(&a.resolutionRetry, reset)
}

func (a *desiredStateReconcilerActor[T]) cancelResolverRestart(reset bool) {
	a.cancelScheduledBackoff(&a.resolverRestart, reset)
}

func (a *desiredStateReconcilerActor[T]) cancelWatcherRestart(reset bool) {
	a.cancelScheduledBackoff(&a.watcherRestart, reset)
}

func (a *desiredStateReconcilerActor[T]) resetDesiredStateResolutionBackoff() {
	a.cancelDesiredStateResolutionRetry(true)
}

func (a *desiredStateReconcilerActor[T]) resetResolverRestartBackoff() {
	a.cancelResolverRestart(true)
}

func (a *desiredStateReconcilerActor[T]) resetWatcherRestartBackoff() {
	a.cancelWatcherRestart(true)
}

func (a *desiredStateReconcilerActor[T]) publishStatus() {
	if !a.activated {
		return
	}
	_ = a.Send(a.Parent(), desiredStateReconcilerStatusChanged{
		generation: a.actorGeneration,
		status:     a.currentStatus(),
	})
}

func (a *desiredStateReconcilerActor[T]) currentStatus() DesiredStateReconcilerStatus {
	lifecycle := DesiredStateReconcilerStarting
	if a.activated {
		lifecycle = DesiredStateReconcilerRunning
	}

	resolver := a.resolverStatus
	resolver.RestartPending = a.resolverRestart.pending
	watcher := a.watcherStatus
	watcher.RestartPending = a.watcherRestart.pending

	availability := runtime.AvailabilityReady
	switch {
	case !a.activated || a.current == nil || resolver.Availability == runtime.AvailabilityUnavailable:
		availability = runtime.AvailabilityUnavailable
	case watcher.Availability != runtime.AvailabilityReady || a.deferred:
		availability = runtime.AvailabilityDegraded
	}

	var snapshotGeneration int64
	if a.current != nil {
		snapshotGeneration = a.current.Generation
	}

	return DesiredStateReconcilerStatus{
		Lifecycle:          lifecycle,
		Availability:       availability,
		ActorGeneration:    a.actorGeneration,
		SnapshotGeneration: snapshotGeneration,
		Revision:           a.revision,
		Resolving:          a.resolving,
		Deferred:           a.deferred,
		Resolver:           resolver,
		Watcher:            watcher,
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

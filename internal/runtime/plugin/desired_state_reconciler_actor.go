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

// artifactDirectoryChanged is emitted by artifactWatcherMeta. The watcher
// generation fences notifications from replaced watcher incarnations.
type artifactDirectoryChanged struct{ generation uint64 }

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

// DesiredStateReconcilerStatus groups the independently managed resolver and
// watcher statuses instead of flattening their fields into the parent.
type DesiredStateReconcilerStatus struct {
	Lifecycle          DesiredStateReconcilerLifecycle
	Availability       runtime.Availability
	ActorGeneration    uint64
	RestartCount       uint64
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

type artifactResolverState struct {
	alias gen.Alias

	generation   uint64
	restartCount uint64
	restart      *scheduledBackoff

	status ArtifactResolverStatus
}

type artifactWatcherState struct {
	alias gen.Alias

	generation   uint64
	restartCount uint64
	restart      *scheduledBackoff

	status ArtifactWatcherStatus
}

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

	resolutionRetry *scheduledBackoff

	resolver artifactResolverState
	watcher  artifactWatcherState

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
		snapshotEvent:   snapshotEvent,
		directory:       directory,
		adapter:         adapter,
		resolutionRetry: newScheduledBackoff(retryMin, retryMax),
		resolver: artifactResolverState{
			restart: newScheduledBackoff(retryMin, retryMax),
			status: ArtifactResolverStatus{
				Lifecycle:    ArtifactResolverStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
		},
		watcher: artifactWatcherState{
			restart: newScheduledBackoff(retryMin, retryMax),
			status: ArtifactWatcherStatus{
				Lifecycle:    ArtifactWatcherStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
		},
	}
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
		if m.generation != a.resolver.generation || a.resolver.alias == (gen.Alias{}) {
			return nil
		}
		a.resolver.status.Lifecycle = ArtifactResolverRunning
		a.resolver.status.Availability = runtime.AvailabilityReady
		a.resolver.status.LastError = ""
		a.resetResolverRestartBackoff()
		a.publishStatus()
		return a.requestResolve()

	case artifactWatcherStarted:
		if m.generation != a.watcher.generation || a.watcher.alias == (gen.Alias{}) {
			return nil
		}
		a.watcher.status.Lifecycle = ArtifactWatcherRunning
		a.watcher.status.Availability = runtime.AvailabilityDegraded
		a.watcher.status.LastError = ""
		a.resetWatcherRestartBackoff()
		a.publishStatus()
		if a.current != nil {
			// The directory may have changed while no watcher incarnation was alive.
			a.dirty = true
			a.cancelDesiredStateResolutionRetry(false)
		}
		return a.requestResolve()

	case artifactWatcherStateChanged:
		if m.generation != a.watcher.generation || a.watcher.alias == (gen.Alias{}) {
			return nil
		}
		a.watcher.status.Lifecycle = ArtifactWatcherRunning
		a.watcher.status.DirectoryReadable = m.directoryReadable
		a.watcher.status.WatchingDirectory = m.watchingDirectory
		a.watcher.status.LastError = m.err
		if m.directoryReadable && m.watchingDirectory {
			a.watcher.status.Availability = runtime.AvailabilityReady
		} else {
			a.watcher.status.Availability = runtime.AvailabilityDegraded
		}
		a.publishStatus()

	case artifactResolved:
		if m.resolverGeneration != a.resolver.generation ||
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
				desiredRevision: a.revision,
				desired:         m.desired,
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
		if m.watcherGeneration != a.watcher.generation ||
			a.watcher.alias == (gen.Alias{}) ||
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
		if !a.resolver.restart.pending ||
			a.resolver.restart.token != m.token ||
			a.resolver.alias != (gen.Alias{}) {
			return nil
		}
		a.resolver.restart.pending = false
		a.resolver.restart.cancel = nil
		return a.startArtifactResolverMeta()

	case artifactWatcherRestart:
		if !a.watcher.restart.pending ||
			a.watcher.restart.token != m.token ||
			a.watcher.alias != (gen.Alias{}) {
			return nil
		}
		a.watcher.restart.pending = false
		a.watcher.restart.cancel = nil
		return a.startArtifactWatcherMeta()

	case gen.MessageDownAlias:
		switch m.Alias {
		case a.resolver.alias:
			a.resolver.alias = gen.Alias{}
			a.resolver.status.Lifecycle = ArtifactResolverRestarting
			a.resolver.status.Availability = runtime.AvailabilityUnavailable
			a.resolver.status.LastError = errorText(m.Reason)
			a.resolving = false
			a.resolveID++
			a.dirty = a.current != nil
			a.cancelDesiredStateResolutionRetry(false)
			a.publishStatus()
			return a.scheduleResolverRestart()

		case a.watcher.alias:
			a.watcher.alias = gen.Alias{}
			a.watcher.status.Lifecycle = ArtifactWatcherRestarting
			a.watcher.status.Availability = runtime.AvailabilityUnavailable
			a.watcher.status.DirectoryReadable = false
			a.watcher.status.WatchingDirectory = false
			a.watcher.status.LastError = errorText(m.Reason)
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
	a.resolver.status.Lifecycle = ArtifactResolverStopped
	a.resolver.status.Availability = runtime.AvailabilityUnavailable
	a.watcher.status.Lifecycle = ArtifactWatcherStopped
	a.watcher.status.Availability = runtime.AvailabilityUnavailable
}

func (a *desiredStateReconcilerActor[T]) startArtifactResolverMeta() error {
	if a.resolver.alias != (gen.Alias{}) {
		return nil
	}

	if a.resolver.generation > 0 {
		a.resolver.restartCount++
	}
	a.resolver.generation++
	a.resolver.status.Lifecycle = ArtifactResolverStarting
	a.resolver.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactResolverMeta[T]{
			directory:  a.directory,
			adapter:    a.adapter,
			generation: a.resolver.generation,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.resolver.status.Lifecycle = ArtifactResolverRestarting
		a.resolver.status.LastError = fmt.Sprintf("spawn artifact resolver meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleResolverRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact resolver meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.resolver.status.Lifecycle = ArtifactResolverRestarting
		a.resolver.status.LastError = fmt.Sprintf("monitor artifact resolver meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleResolverRestart(); retryErr != nil {
			return fmt.Errorf("monitor artifact resolver meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	a.resolver.alias = alias
	return nil
}

func (a *desiredStateReconcilerActor[T]) startArtifactWatcherMeta() error {
	if a.watcher.alias != (gen.Alias{}) {
		return nil
	}

	if a.watcher.generation > 0 {
		a.watcher.restartCount++
	}
	a.watcher.generation++
	a.watcher.status.Lifecycle = ArtifactWatcherStarting
	a.watcher.status.Availability = runtime.AvailabilityUnavailable
	a.watcher.status.DirectoryReadable = false
	a.watcher.status.WatchingDirectory = false
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactWatcherMeta{
			directory:  a.directory,
			generation: a.watcher.generation,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.watcher.status.Lifecycle = ArtifactWatcherRestarting
		a.watcher.status.LastError = fmt.Sprintf("spawn artifact watcher meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleWatcherRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact watcher meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.watcher.status.Lifecycle = ArtifactWatcherRestarting
		a.watcher.status.LastError = fmt.Sprintf("monitor artifact watcher meta: %v", err)
		a.publishStatus()
		if retryErr := a.scheduleWatcherRestart(); retryErr != nil {
			return fmt.Errorf("monitor artifact watcher meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	a.watcher.alias = alias
	return nil
}

func (a *desiredStateReconcilerActor[T]) stopArtifactResolverMeta(reason error) {
	if a.resolver.alias == (gen.Alias{}) {
		return
	}
	alias := a.resolver.alias
	a.resolver.alias = gen.Alias{}
	_ = a.DemonitorAlias(alias)
	_ = a.SendExitMeta(alias, reason)
}

func (a *desiredStateReconcilerActor[T]) stopArtifactWatcherMeta(reason error) {
	if a.watcher.alias == (gen.Alias{}) {
		return
	}
	alias := a.watcher.alias
	a.watcher.alias = gen.Alias{}
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
		a.resolver.alias == (gen.Alias{}) {
		return nil
	}

	a.resolving = true
	a.dirty = false
	a.resolveID++
	a.publishStatus()
	if err := a.Send(a.resolver.alias, artifactResolve{
		id:                 a.resolveID,
		resolverGeneration: a.resolver.generation,
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
	if a.resolver.restart.pending {
		return nil
	}

	delay := a.resolver.restart.strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact resolver restart backoff stopped")
	}

	a.resolver.restart.token++
	token := a.resolver.restart.token
	cancel, err := a.SendAfter(a.PID(), artifactResolverRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact resolver restart: %w", err)
	}
	a.resolver.restart.pending = true
	a.resolver.restart.cancel = cancel
	a.resolver.status.Lifecycle = ArtifactResolverRestarting
	a.resolver.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

func (a *desiredStateReconcilerActor[T]) scheduleWatcherRestart() error {
	if a.watcher.restart.pending {
		return nil
	}

	delay := a.watcher.restart.strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact watcher restart backoff stopped")
	}

	a.watcher.restart.token++
	token := a.watcher.restart.token
	cancel, err := a.SendAfter(a.PID(), artifactWatcherRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact watcher restart: %w", err)
	}
	a.watcher.restart.pending = true
	a.watcher.restart.cancel = cancel
	a.watcher.status.Lifecycle = ArtifactWatcherRestarting
	a.watcher.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

func (a *desiredStateReconcilerActor[T]) cancelDesiredStateResolutionRetry(reset bool) {
	a.resolutionRetry.cancelScheduled(reset)
}

func (a *desiredStateReconcilerActor[T]) cancelResolverRestart(reset bool) {
	a.resolver.restart.cancelScheduled(reset)
}

func (a *desiredStateReconcilerActor[T]) cancelWatcherRestart(reset bool) {
	a.watcher.restart.cancelScheduled(reset)
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

	resolver := a.resolver.status
	resolver.Generation = a.resolver.generation
	resolver.RestartCount = a.resolver.restartCount
	resolver.RestartPending = a.resolver.restart != nil && a.resolver.restart.pending
	watcher := a.watcher.status
	watcher.Generation = a.watcher.generation
	watcher.RestartCount = a.watcher.restartCount
	watcher.RestartPending = a.watcher.restart != nil && a.watcher.restart.pending

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

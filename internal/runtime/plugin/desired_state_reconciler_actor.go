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

// MessageDesiredStateReconcilerActivate gives a replacement reconciler a
// revision base newer than the last state accepted by its supervisor.
type MessageDesiredStateReconcilerActivate struct{ revisionBase uint64 }

type MessageDesiredStateResolutionRetry struct{ token uint64 }
type MessageArtifactResolverRestart struct{ token uint64 }
type MessageArtifactWatcherRestart struct{ token uint64 }

type MessageResolveArtifacts struct {
	incarnation uint64
	snapshot    snapshot.Snapshot
}

type MessageArtifactResolutionResult struct {
	incarnation        uint64
	snapshotGeneration int64
	desired            map[string]MessageApplyRouterDesiredState
	deferred           bool
	complete           bool
}

type MessageArtifactResolverStarted struct{ incarnation uint64 }
type MessageArtifactWatcherStarted struct{ incarnation uint64 }

// MessageArtifactDirectoryChanged is emitted by artifactWatcherMeta. Its
// incarnation fences notifications from replaced watcher processes.
type MessageArtifactDirectoryChanged struct{ incarnation uint64 }

type MessageArtifactWatcherStateChanged struct {
	incarnation       uint64
	directoryReadable bool
	watchingDirectory bool
	err               error
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
	Incarnation        uint64
	RestartCount       uint64
	LastError          error
	SnapshotGeneration int64
	Revision           uint64
	Resolving          bool
	Deferred           bool
	Resolver           ArtifactResolverStatus
	Watcher            ArtifactWatcherStatus
}

type MessageDesiredStateReconcilerStatusChanged struct{ status DesiredStateReconcilerStatus }

type artifactResolverState struct {
	alias gen.Alias

	incarnation  uint64
	restartCount uint64
	restart      *runtime.ScheduledBackoff

	status ArtifactResolverStatus
}

type artifactWatcherState struct {
	alias gen.Alias

	incarnation  uint64
	restartCount uint64
	restart      *runtime.ScheduledBackoff

	status ArtifactWatcherStatus
}

// desiredStateReconcilerActor subscribes to the buffered snapshot event and is
// the stable owner of the current snapshot, desired-state revisions, resolution
// coalescing, and all retry policies. It independently replaces its resolver and
// watcher meta-processes so an expected I/O-process failure does not discard the
// actor's state or event subscription.
type desiredStateReconcilerActor[T plugin.Syncable] struct {
	act.Actor

	lifecycle DesiredStateReconcilerLifecycle
	revision  uint64

	snapshotEvent gen.Event
	directory     string
	adapter       *plugin.PluginAdapter[T]

	resolutionRetry *runtime.ScheduledBackoff

	resolver artifactResolverState
	watcher  artifactWatcherState

	current   *snapshot.Snapshot
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
		lifecycle:       DesiredStateReconcilerStarting,
		snapshotEvent:   snapshotEvent,
		directory:       directory,
		adapter:         adapter,
		resolutionRetry: runtime.NewScheduledBackoff(retryMin, retryMax),
		resolver: artifactResolverState{
			restart: runtime.NewScheduledBackoff(retryMin, retryMax),
			status: ArtifactResolverStatus{
				Lifecycle:    ArtifactResolverStarting,
				Availability: runtime.AvailabilityUnavailable,
			},
		},
		watcher: artifactWatcherState{
			restart: runtime.NewScheduledBackoff(retryMin, retryMax),
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
	case MessageDesiredStateReconcilerActivate:
		if a.lifecycle == DesiredStateReconcilerRunning {
			return fmt.Errorf("desired-state reconciler already activated")
		}

		a.revision = m.revisionBase
		a.lifecycle = DesiredStateReconcilerRunning
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

	case MessageArtifactResolverStarted:
		if m.incarnation != a.resolver.incarnation || a.resolver.alias == (gen.Alias{}) {
			return nil
		}
		a.resolver.status.Lifecycle = ArtifactResolverRunning
		a.resolver.status.Availability = runtime.AvailabilityReady
		a.resolver.status.LastError = nil
		a.resetResolverRestartBackoff()
		a.publishStatus()
		return a.requestResolve()

	case MessageArtifactWatcherStarted:
		if m.incarnation != a.watcher.incarnation || a.watcher.alias == (gen.Alias{}) {
			return nil
		}
		a.watcher.status.Lifecycle = ArtifactWatcherRunning
		a.watcher.status.Availability = runtime.AvailabilityDegraded
		a.watcher.status.LastError = nil
		a.resetWatcherRestartBackoff()
		a.publishStatus()
		if a.current != nil {
			// The directory may have changed while no watcher incarnation was alive.
			a.dirty = true
			a.cancelDesiredStateResolutionRetry(false)
		}
		return a.requestResolve()

	case MessageArtifactWatcherStateChanged:
		if m.incarnation != a.watcher.incarnation || a.watcher.alias == (gen.Alias{}) {
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

	case MessageArtifactResolutionResult:
		if m.incarnation != a.resolver.incarnation || !a.resolving {
			return nil
		}
		a.resolving = false
		a.resolver.status.Availability = runtime.AvailabilityReady
		a.resolver.status.LastError = nil

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
		if !m.complete {
			a.dirty = true
			a.deferred = true
			a.publishStatus()
			return a.scheduleDesiredStateResolutionRetry()
		}

		a.revision++
		a.deferred = m.deferred
		if err := a.Send(a.Parent(), MessageApplyDesiredState{
			desired: MessageApplyCatalogDesiredState{
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

	case MessageArtifactDirectoryChanged:
		if m.incarnation != a.watcher.incarnation ||
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

	case MessageDesiredStateResolutionRetry:
		if !a.resolutionRetry.Pending || a.resolutionRetry.Token != m.token {
			return nil
		}
		a.resolutionRetry.Pending = false
		a.resolutionRetry.Cancel = nil
		a.dirty = true
		return a.requestResolve()

	case MessageArtifactResolverRestart:
		if !a.resolver.restart.Pending ||
			a.resolver.restart.Token != m.token ||
			a.resolver.alias != (gen.Alias{}) {
			return nil
		}
		a.resolver.restart.Pending = false
		a.resolver.restart.Cancel = nil
		return a.startArtifactResolverMeta()

	case MessageArtifactWatcherRestart:
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
			a.resolver.status.Lifecycle = ArtifactResolverRestarting
			a.resolver.status.Availability = runtime.AvailabilityUnavailable
			a.resolver.status.LastError = m.Reason
			a.resolving = false
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
			a.watcher.status.LastError = m.Reason
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

func (a *desiredStateReconcilerActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("actorruntime: unsupported desired-state reconciler call %T", request)
}

func (a *desiredStateReconcilerActor[T]) Terminate(error) {
	a.lifecycle = DesiredStateReconcilerStopped
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

	if a.resolver.incarnation > 0 {
		a.resolver.restartCount++
	}
	a.resolver.incarnation++
	a.resolver.status.Lifecycle = ArtifactResolverStarting
	a.resolver.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactResolverMeta[T]{
			directory:   a.directory,
			adapter:     a.adapter,
			incarnation: a.resolver.incarnation,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.resolver.status.Lifecycle = ArtifactResolverRestarting
		a.resolver.status.LastError = fmt.Errorf("%w: spawn meta: %w", runtime.ErrArtifactResolve, err)
		a.publishStatus()
		if retryErr := a.scheduleResolverRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact resolver meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.resolver.status.Lifecycle = ArtifactResolverRestarting
		a.resolver.status.LastError = fmt.Errorf("%w: monitor meta: %w", runtime.ErrArtifactResolve, err)
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

	if a.watcher.incarnation > 0 {
		a.watcher.restartCount++
	}
	a.watcher.incarnation++
	a.watcher.status.Lifecycle = ArtifactWatcherStarting
	a.watcher.status.Availability = runtime.AvailabilityUnavailable
	a.watcher.status.DirectoryReadable = false
	a.watcher.status.WatchingDirectory = false
	a.publishStatus()
	alias, err := a.SpawnMeta(
		&artifactWatcherMeta{
			directory:   a.directory,
			incarnation: a.watcher.incarnation,
		},
		gen.MetaOptions{},
	)
	if err != nil {
		a.watcher.status.Lifecycle = ArtifactWatcherRestarting
		a.watcher.status.LastError = fmt.Errorf("%w: spawn meta: %w", runtime.ErrArtifactWatch, err)
		a.publishStatus()
		if retryErr := a.scheduleWatcherRestart(); retryErr != nil {
			return fmt.Errorf("spawn artifact watcher meta: %v; schedule restart: %w", err, retryErr)
		}
		return nil
	}
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		a.watcher.status.Lifecycle = ArtifactWatcherRestarting
		a.watcher.status.LastError = fmt.Errorf("%w: monitor meta: %w", runtime.ErrArtifactWatch, err)
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

	a.current = snap.Clone()
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
	a.publishStatus()
	snap := a.current.Clone()
	if err := a.Send(a.resolver.alias, MessageResolveArtifacts{
		incarnation: a.resolver.incarnation,
		snapshot:    *snap,
	}); err != nil {
		a.resolving = false
		a.dirty = true
		a.resolver.status.LastError = fmt.Errorf("%w: queue request: %w", runtime.ErrArtifactResolve, err)
		a.resolver.status.Availability = runtime.AvailabilityDegraded
		a.publishStatus()
		if retryErr := a.scheduleDesiredStateResolutionRetry(); retryErr != nil {
			return fmt.Errorf("send artifact resolve request: %v; schedule retry: %w", err, retryErr)
		}
	}
	return nil
}

func (a *desiredStateReconcilerActor[T]) scheduleDesiredStateResolutionRetry() error {
	if a.resolutionRetry.Pending {
		return nil
	}

	delay := a.resolutionRetry.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("desired-state resolution retry backoff stopped")
	}

	a.resolutionRetry.Token++
	token := a.resolutionRetry.Token
	cancel, err := a.SendAfter(a.PID(), MessageDesiredStateResolutionRetry{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule desired-state resolution retry: %w", err)
	}
	a.resolutionRetry.Pending = true
	a.resolutionRetry.Cancel = cancel
	return nil
}

func (a *desiredStateReconcilerActor[T]) scheduleResolverRestart() error {
	if a.resolver.restart.Pending {
		return nil
	}

	delay := a.resolver.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact resolver restart backoff stopped")
	}

	a.resolver.restart.Token++
	token := a.resolver.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageArtifactResolverRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact resolver restart: %w", err)
	}
	a.resolver.restart.Pending = true
	a.resolver.restart.Cancel = cancel
	a.resolver.status.Lifecycle = ArtifactResolverRestarting
	a.resolver.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

func (a *desiredStateReconcilerActor[T]) scheduleWatcherRestart() error {
	if a.watcher.restart.Pending {
		return nil
	}

	delay := a.watcher.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("artifact watcher restart backoff stopped")
	}

	a.watcher.restart.Token++
	token := a.watcher.restart.Token
	cancel, err := a.SendAfter(a.PID(), MessageArtifactWatcherRestart{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule artifact watcher restart: %w", err)
	}
	a.watcher.restart.Pending = true
	a.watcher.restart.Cancel = cancel
	a.watcher.status.Lifecycle = ArtifactWatcherRestarting
	a.watcher.status.Availability = runtime.AvailabilityUnavailable
	a.publishStatus()
	return nil
}

func (a *desiredStateReconcilerActor[T]) cancelDesiredStateResolutionRetry(reset bool) {
	a.resolutionRetry.CancelScheduled(reset)
}

func (a *desiredStateReconcilerActor[T]) cancelResolverRestart(reset bool) {
	a.resolver.restart.CancelScheduled(reset)
}

func (a *desiredStateReconcilerActor[T]) cancelWatcherRestart(reset bool) {
	a.watcher.restart.CancelScheduled(reset)
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
	if a.lifecycle != DesiredStateReconcilerRunning {
		return
	}
	_ = a.Send(a.Parent(), MessageDesiredStateReconcilerStatusChanged{
		status: a.currentStatus(),
	})
}

func (a *desiredStateReconcilerActor[T]) currentStatus() DesiredStateReconcilerStatus {
	resolver := a.resolver.status
	resolver.Incarnation = a.resolver.incarnation
	resolver.RestartCount = a.resolver.restartCount
	resolver.RestartPending = a.resolver.restart != nil && a.resolver.restart.Pending
	watcher := a.watcher.status
	watcher.Incarnation = a.watcher.incarnation
	watcher.RestartCount = a.watcher.restartCount
	watcher.RestartPending = a.watcher.restart != nil && a.watcher.restart.Pending

	availability := runtime.AvailabilityReady
	switch {
	case a.lifecycle != DesiredStateReconcilerRunning || a.current == nil || resolver.Availability == runtime.AvailabilityUnavailable:
		availability = runtime.AvailabilityUnavailable
	case watcher.Availability != runtime.AvailabilityReady || a.deferred:
		availability = runtime.AvailabilityDegraded
	}

	var snapshotGeneration int64
	if a.current != nil {
		snapshotGeneration = a.current.Generation
	}

	return DesiredStateReconcilerStatus{
		Lifecycle:          a.lifecycle,
		Availability:       availability,
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

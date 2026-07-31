package plugin

import (
	"context"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
)

// RuntimeLifecycle describes the complete plugin runtime subtree.
type RuntimeLifecycle string

const (
	RuntimeStarting RuntimeLifecycle = "starting"
	RuntimeRunning  RuntimeLifecycle = "running"
	RuntimeDraining RuntimeLifecycle = "draining"
	RuntimeStopped  RuntimeLifecycle = "stopped"
)

// RuntimeStatus is the authoritative public status published by the runtime
// supervisor. Child actor statuses are merged with supervisor-owned
// incarnation and restart metadata before they are exposed here.
type RuntimeStatus struct {
	Lifecycle       RuntimeLifecycle
	Availability    runtime.Availability
	DesiredRevision uint64
	Catalog         CatalogStatus
	Reconciler      DesiredStateReconcilerStatus
}

func (s RuntimeStatus) clone() RuntimeStatus {
	clone := s
	clone.Catalog = s.Catalog.clone()
	return clone
}

const (
	runtimeChildRestartIntensity uint16 = 5
	runtimeChildRestartPeriod    uint16 = 5
)

// RuntimeEvents identifies the stable buffered events owned by one runtime
// supervisor subtree.
type RuntimeEvents struct {
	Status gen.Event
}

// RuntimeEventsFor derives the public event names from the runtime name in the
// same way that actorsnapshot.EventsFor derives snapshot-reader events.
func RuntimeEventsFor(node gen.Node, name gen.Atom) RuntimeEvents {
	return RuntimeEvents{
		Status: gen.Event{
			Name: gen.Atom(string(name) + "-status"),
			Node: node.Name(),
		},
	}
}

type runtimeSupervisorOptions[T plugin.Syncable] struct {
	Name          gen.Atom
	Dependencies  actorDependencies[T]
	SnapshotEvent gen.Event
	Directory     string
	Stopped       chan<- error
}

type runtimeDrainWaiter struct {
	pid gen.PID
	ref gen.Ref
}

func (t runtimeDrainWaiter) alive() bool { return t.ref.IsAlive() }

type runtimeDrainReply struct{ Err error }

type runtimeInvocationState struct {
	catalogPID gen.PID
	result     *runtime.AsyncResult
}

type runtimeReconcilerState struct {
	pid             gen.PID
	actorGeneration uint64
	restartCount    uint64
	status          DesiredStateReconcilerStatus
}

type runtimeCatalogState struct {
	pid             gen.PID
	actorGeneration uint64
	restartCount    uint64
	lastEpoch       uint64
	status          CatalogStatus
}

type runtimeSupervisor[T plugin.Syncable] struct {
	act.Supervisor

	opts runtimeSupervisorOptions[T]

	events      RuntimeEvents
	statusToken gen.Ref

	reconciler runtimeReconcilerState
	catalog    runtimeCatalogState

	desiredState        catalogApplyDesired
	inFlightInvocations map[uint64]runtimeInvocationState
	drainWaiters        []runtimeDrainWaiter

	draining    bool
	everRunning bool
	liveStatus  RuntimeStatus
}

type runtimeApplyDesired struct {
	generation uint64
	desired    catalogApplyDesired
}

type runtimeSubmit[T plugin.Syncable] struct {
	callID               uint64
	context              context.Context
	pluginID, rolloutKey string
	fn                   func(context.Context, T) error
	shadow               bool
	result               *runtime.AsyncResult
}

type runtimeDrain struct{}
type runtimeGetStatus struct{}

func newRuntimeSupervisor[T plugin.Syncable](
	opts runtimeSupervisorOptions[T],
) gen.ProcessBehavior {
	return &runtimeSupervisor[T]{opts: opts}
}

func (s *runtimeSupervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	if s.opts.Name == "" ||
		s.opts.Dependencies.adapter == nil ||
		s.opts.SnapshotEvent.Name == "" ||
		s.opts.Directory == "" {
		return act.SupervisorSpec{}, fmt.Errorf(
			"actorruntime: name, adapter, snapshot event, and directory are required",
		)
	}

	s.events = RuntimeEventsFor(s.Node(), s.opts.Name)
	s.inFlightInvocations = make(map[uint64]runtimeInvocationState)
	s.catalog.status = newCatalogStatus(0, 0, "")
	s.reconciler.status = newDesiredStateReconcilerStatus(0, 0, "")
	s.reconcileStatus()

	token, err := s.RegisterEvent(s.events.Status.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf(
			"register runtime status event %s: %w",
			s.events.Status.Name,
			err,
		)
	}
	s.statusToken = token
	s.publishStatus()

	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyPermanent,
			Intensity: runtimeChildRestartIntensity,
			Period:    runtimeChildRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{
			{
				Name: s.reconcilerActorName(),
				Factory: func() gen.ProcessBehavior {
					return newDesiredStateReconcilerActor(
						s.opts.SnapshotEvent,
						s.opts.Directory,
						s.opts.Dependencies.adapter,
						s.opts.Dependencies.retryMin,
						s.opts.Dependencies.retryMax,
					)
				},
				Options: gen.ProcessOptions{},
			},
			{
				Name: s.catalogActorName(),
				Factory: func() gen.ProcessBehavior {
					return newCatalogActor(s.opts.Dependencies)
				},
				Options: gen.ProcessOptions{},
			},
		},
	}, nil
}

// HandleCall remains control-plane only. Plugin execution enters through
// runtimeSubmit in HandleMessage and never blocks an Ergo Call callback.
func (s *runtimeSupervisor[T]) HandleCall(
	from gen.PID,
	ref gen.Ref,
	request any,
) (any, error) {
	switch request.(type) {
	case runtimeDrain:
		s.drainWaiters = append(s.drainWaiters, runtimeDrainWaiter{pid: from, ref: ref})
		if s.draining {
			return nil, nil
		}

		s.draining = true
		s.reconcileStatus()
		s.publishStatus()
		if s.catalog.pid != (gen.PID{}) {
			_ = s.Send(s.catalog.pid, catalogDrain{})
		}
		return nil, nil

	case runtimeGetStatus:
		return s.liveStatus.clone(), nil

	default:
		return runtimeDrainReply{
			Err: fmt.Errorf("actorruntime: unsupported supervisor call %T", request),
		}, nil
	}
}

func (s *runtimeSupervisor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case runtimeSubmit[T]:
		if s.draining || s.catalog.pid == (gen.PID{}) {
			m.result.Complete(ErrPluginUnavailable)
			return nil
		}
		if err := m.context.Err(); err != nil {
			m.result.Complete(err)
			return nil
		}

		catalogPID := s.catalog.pid
		s.inFlightInvocations[m.callID] = runtimeInvocationState{
			catalogPID: catalogPID,
			result:     m.result,
		}
		call := invokeCall[T]{
			callID:     m.callID,
			context:    m.context,
			pluginID:   m.pluginID,
			rolloutKey: m.rolloutKey,
			fn:         m.fn,
			shadow:     m.shadow,
		}
		if err := s.Send(catalogPID, call); err != nil {
			s.finishCall(m.callID, ErrPluginUnavailable)
			_ = s.Node().SendExit(
				catalogPID,
				fmt.Errorf("forward invocation to catalog: %w", err),
			)
		}

	case desiredStateReconcilerStatusChanged:
		if from != s.reconciler.pid ||
			m.generation != s.reconciler.actorGeneration {
			return nil
		}
		s.mergeReconcilerStatus(m.status)
		s.reconcileStatus()
		s.publishStatus()

	case runtimeApplyDesired:
		if from != s.reconciler.pid ||
			m.generation != s.reconciler.actorGeneration ||
			s.draining ||
			m.desired.desiredRevision <= s.desiredState.desiredRevision {
			return nil
		}

		s.desiredState = m.desired
		if s.catalog.pid != (gen.PID{}) {
			if err := s.Send(s.catalog.pid, m.desired); err != nil {
				_ = s.Node().SendExit(
					s.catalog.pid,
					fmt.Errorf("apply desired state to catalog: %w", err),
				)
			}
		}
		s.reconcileStatus()
		s.publishStatus()

	case cancelCall:
		call, ok := s.inFlightInvocations[m.callID]
		if !ok {
			return nil
		}
		if err := s.Send(call.catalogPID, m); err != nil {
			s.finishCall(m.callID, m.err)
		}

	case callCompleted:
		s.finishCall(m.callID, m.err)

	case catalogStatusChanged:
		if from != s.catalog.pid ||
			m.pid != s.catalog.pid ||
			m.generation != s.catalog.actorGeneration ||
			m.epoch <= s.catalog.lastEpoch {
			return nil
		}

		s.catalog.lastEpoch = m.epoch
		s.mergeCatalogStatus(m.status)
		s.reconcileStatus()
		s.publishStatus()

	case catalogDrained:
		if !s.draining ||
			from != s.catalog.pid ||
			m.pid != s.catalog.pid ||
			m.generation != s.catalog.actorGeneration {
			return nil
		}

		for callID := range s.inFlightInvocations {
			s.finishCall(callID, ErrPluginUnavailable)
		}
		for _, waiter := range s.drainWaiters {
			if waiter.alive() {
				_ = s.SendResponse(waiter.pid, waiter.ref, runtimeDrainReply{})
			}
		}
		s.drainWaiters = nil
		return gen.TerminateReasonNormal
	}
	return nil
}

func (s *runtimeSupervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	switch name {
	case s.reconcilerActorName():
		return s.startReconcilerIncarnation(pid)

	case s.catalogActorName():
		return s.startCatalogIncarnation(pid)
	}
	return nil
}

func (s *runtimeSupervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	switch name {
	case s.reconcilerActorName():
		s.retireReconcilerIncarnation(pid, reason)

	case s.catalogActorName():
		s.retireCatalogIncarnation(pid, reason)
	}

	s.reconcileStatus()
	s.publishStatus()
	return nil
}

func (s *runtimeSupervisor[T]) Terminate(reason error) {
	for callID := range s.inFlightInvocations {
		s.finishCall(callID, ErrPluginUnavailable)
	}

	s.liveStatus.Lifecycle = RuntimeStopped
	s.liveStatus.Availability = runtime.AvailabilityUnavailable
	s.liveStatus.Catalog.Lifecycle = CatalogStopped
	s.liveStatus.Catalog.Availability = runtime.AvailabilityUnavailable
	s.liveStatus.Reconciler.Lifecycle = DesiredStateReconcilerStopped
	s.liveStatus.Reconciler.Availability = runtime.AvailabilityUnavailable
	s.publishStatus()

	if s.opts.Stopped == nil {
		return
	}
	select {
	case s.opts.Stopped <- reason:
	default:
	}
}

func (s *runtimeSupervisor[T]) startReconcilerIncarnation(pid gen.PID) error {
	state := &s.reconciler
	if state.actorGeneration > 0 {
		state.restartCount++
	}

	state.actorGeneration++
	state.pid = pid
	state.status = newDesiredStateReconcilerStatus(
		state.actorGeneration,
		state.restartCount,
		state.status.ActorLastError,
	)
	s.reconcileStatus()
	s.publishStatus()

	if err := s.Send(pid, desiredStateReconcilerActivate{
		generation:   state.actorGeneration,
		revisionBase: s.desiredState.desiredRevision,
	}); err != nil {
		_ = s.Node().SendExit(
			pid,
			fmt.Errorf("activate desired-state reconciler: %w", err),
		)
	}
	return nil
}

func (s *runtimeSupervisor[T]) startCatalogIncarnation(pid gen.PID) error {
	state := &s.catalog
	if state.actorGeneration > 0 {
		state.restartCount++
	}

	state.actorGeneration++
	state.pid = pid
	state.lastEpoch = 0
	state.status = newCatalogStatus(
		state.actorGeneration,
		state.restartCount,
		state.status.ActorLastError,
	)
	s.reconcileStatus()
	s.publishStatus()

	if err := s.Send(pid, catalogActivate{generation: state.actorGeneration}); err != nil {
		_ = s.Node().SendExit(pid, fmt.Errorf("activate catalog: %w", err))
		return nil
	}
	if s.desiredState.desiredRevision != 0 {
		if err := s.Send(pid, s.desiredState); err != nil {
			_ = s.Node().SendExit(pid, fmt.Errorf("replay desired state to catalog: %w", err))
			return nil
		}
	}
	if s.draining {
		if err := s.Send(pid, catalogDrain{}); err != nil {
			_ = s.Node().SendExit(
				pid,
				fmt.Errorf("drain replacement catalog: %w", err),
			)
		}
	}
	return nil
}

func (s *runtimeSupervisor[T]) retireReconcilerIncarnation(
	pid gen.PID,
	reason error,
) {
	state := &s.reconciler
	if state.pid != pid {
		return
	}

	state.pid = gen.PID{}
	state.status.Lifecycle = DesiredStateReconcilerRestarting
	state.status.Availability = runtime.AvailabilityUnavailable
	state.status.ActorGeneration = state.actorGeneration
	state.status.RestartCount = state.restartCount
	state.status.ActorLastError = errorText(reason)
	state.status.Resolver.Lifecycle = ArtifactResolverStopped
	state.status.Resolver.Availability = runtime.AvailabilityUnavailable
	state.status.Resolver.RestartPending = false
	state.status.Watcher.Lifecycle = ArtifactWatcherStopped
	state.status.Watcher.Availability = runtime.AvailabilityUnavailable
	state.status.Watcher.RestartPending = false
}

func (s *runtimeSupervisor[T]) retireCatalogIncarnation(
	pid gen.PID,
	reason error,
) {
	state := &s.catalog
	if state.pid != pid {
		return
	}

	state.pid = gen.PID{}
	state.lastEpoch = 0
	state.status.Lifecycle = CatalogRestarting
	state.status.Availability = runtime.AvailabilityUnavailable
	state.status.ActorGeneration = state.actorGeneration
	state.status.RestartCount = state.restartCount
	state.status.RestartPending = true
	state.status.ActorLastError = errorText(reason)

	for callID, call := range s.inFlightInvocations {
		if call.catalogPID == pid {
			s.finishCall(callID, ErrPluginUnavailable)
		}
	}
}

func (s *runtimeSupervisor[T]) finishCall(callID uint64, err error) {
	call, ok := s.inFlightInvocations[callID]
	if !ok {
		return
	}
	delete(s.inFlightInvocations, callID)
	call.result.Complete(err)
}

func (s *runtimeSupervisor[T]) mergeReconcilerStatus(status DesiredStateReconcilerStatus) {
	state := &s.reconciler
	status.ActorGeneration = state.actorGeneration
	status.RestartCount = state.restartCount
	if status.Lifecycle == DesiredStateReconcilerRunning {
		status.ActorLastError = ""
	} else {
		status.ActorLastError = state.status.ActorLastError
	}
	state.status = status
}

func (s *runtimeSupervisor[T]) mergeCatalogStatus(status CatalogStatus) {
	state := &s.catalog
	next := status.clone()
	next.ActorGeneration = state.actorGeneration
	next.RestartCount = state.restartCount
	next.RestartPending = false
	if next.Lifecycle == CatalogRunning {
		next.ActorLastError = ""
		s.everRunning = true
	} else {
		next.ActorLastError = state.status.ActorLastError
	}
	state.status = next
}

func (s *runtimeSupervisor[T]) reconcileStatus() {
	s.liveStatus = RuntimeStatus{
		Lifecycle:       s.runtimeLifecycle(),
		Availability:    s.runtimeAvailability(),
		DesiredRevision: s.desiredState.desiredRevision,
		Catalog:         s.catalog.status.clone(),
		Reconciler:      s.reconciler.status,
	}
}

func (s *runtimeSupervisor[T]) publishStatus() {
	if s.events.Status.Name == "" || s.statusToken == (gen.Ref{}) {
		return
	}
	_ = s.SendEvent(s.events.Status.Name, s.statusToken, s.liveStatus.clone())
}

func newCatalogStatus(actorGeneration uint64, restartCount uint64, lastError string) CatalogStatus {
	return CatalogStatus{
		Lifecycle:       CatalogStarting,
		Availability:    runtime.AvailabilityUnavailable,
		ActorGeneration: actorGeneration,
		RestartCount:    restartCount,
		ActorLastError:  lastError,
		Routers:         make(map[string]RouterStatus),
	}
}

func newDesiredStateReconcilerStatus(actorGeneration uint64, restartCount uint64, lastError string) DesiredStateReconcilerStatus {
	return DesiredStateReconcilerStatus{
		Lifecycle:       DesiredStateReconcilerStarting,
		Availability:    runtime.AvailabilityUnavailable,
		ActorGeneration: actorGeneration,
		RestartCount:    restartCount,
		ActorLastError:  lastError,
		Resolver: ArtifactResolverStatus{
			Lifecycle:    ArtifactResolverStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
		Watcher: ArtifactWatcherStatus{
			Lifecycle:    ArtifactWatcherStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
}

func (s *runtimeSupervisor[T]) runtimeAvailability() runtime.Availability {
	if s.catalog.status.Availability == runtime.AvailabilityUnavailable {
		return runtime.AvailabilityUnavailable
	}
	if s.catalog.status.Availability != runtime.AvailabilityReady ||
		s.reconciler.status.Availability != runtime.AvailabilityReady {
		return runtime.AvailabilityDegraded
	}
	return runtime.AvailabilityReady
}

func (s *runtimeSupervisor[T]) runtimeLifecycle() RuntimeLifecycle {
	switch {
	case s.draining:
		return RuntimeDraining
	case s.everRunning:
		return RuntimeRunning
	default:
		return RuntimeStarting
	}
}

func (s *runtimeSupervisor[T]) reconcilerActorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-desired-state-reconciler")
}

func (s *runtimeSupervisor[T]) catalogActorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-catalog")
}

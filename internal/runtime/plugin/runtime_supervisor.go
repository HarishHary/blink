package plugin

import (
	"context"
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

const (
	supervisorChildRestartIntensity uint16 = 5
	supervisorChildRestartPeriod    uint16 = 5
)

// SupervisorLifecycle describes the complete plugin runtime subtree.
type SupervisorLifecycle string

const (
	SupervisorStarting SupervisorLifecycle = "starting"
	SupervisorRunning  SupervisorLifecycle = "running"
	SupervisorDraining SupervisorLifecycle = "draining"
)

// SupervisorTransitionPhase describes desired-state transition progress.
type SupervisorTransitionPhase uint8

const (
	SupervisorTransitionIdle SupervisorTransitionPhase = iota
	SupervisorTransitionPreparing
	SupervisorTransitionAwaitingFreshness
	SupervisorTransitionAwaitingProjection
)

// SupervisorStatus is the authoritative public status published by the runtime
// supervisor.
type SupervisorStatus struct {
	Lifecycle       SupervisorLifecycle
	Availability    runtime.Availability
	DesiredRevision uint64
	Transition      SupervisorTransitionPhase
	Catalog         catalogActorStatus
	Reconciler      reconcilerActorStatus
}

// clone returns an independent copy of the runtime status.
func (s SupervisorStatus) clone() SupervisorStatus {
	clone := s
	clone.Catalog = s.Catalog.clone()
	return clone
}

// ---------------------------------------------------------------------------
// Runtime Events
// ---------------------------------------------------------------------------

// RuntimeEvents identifies the stable buffered events owned by one runtime
// supervisor subtree.
type RuntimeEvents struct {
	Status gen.Event
}

// RuntimeEventsFor derives the public event names from the runtime name in the
// same way that actorsnapshot.EventsFor derives snapshot-reader events.
func RuntimeEventsFor(node gen.Node, name gen.Atom) RuntimeEvents {
	return RuntimeEvents{
		Status: gen.Event{Name: gen.Atom(string(name) + "-status"), Node: node.Name()},
	}
}

// ---------------------------------------------------------------------------
// Supervisor Configuration
// ---------------------------------------------------------------------------

// runtimeDrainWaiter tracks a caller waiting for runtime drain completion.
type runtimeDrainWaiter struct {
	pid gen.PID
	ref gen.Ref
}

// alive reports whether the drain waiter can still receive a response.
func (t runtimeDrainWaiter) alive() bool { return t.ref.IsAlive() }

// ---------------------------------------------------------------------------
// Runtime Control Messages
// ---------------------------------------------------------------------------

// DrainRequest asks the runtime supervisor to drain.
type DrainRequest struct{}

// DrainResponse reports completion of a runtime drain request.
type DrainResponse struct{ Err error }

// SupervisorStatusRequest asks the runtime supervisor for its current status.
type SupervisorStatusRequest struct{}

// SupervisorStatusResponse contains the current runtime status.
type SupervisorStatusResponse struct{ Status SupervisorStatus }

// SupervisorStateRequest asks the runtime supervisor for its ready generation.
type SupervisorStateRequest struct{}

// SupervisorStateResponse contains the ready runtime generation.
type SupervisorStateResponse struct{ Generation int64 }

// MessageProjectionCommitRetry triggers a deferred projection commit retry.
type MessageProjectionCommitRetry struct{ token uint64 }

// MessageProjectionCommitDeadline marks a projection commit attempt as expired.
type MessageProjectionCommitDeadline struct {
	token         uint64
	generation    int64
	projectionPID gen.PID
}

// ---------------------------------------------------------------------------
// Runtime State
// ---------------------------------------------------------------------------

// runtimeCall tracks an in-flight runtime call.
type runtimeCall struct {
	catalog gen.PID
	result  *runtime.AsyncResult
	cancel  context.CancelFunc
}

// ---------------------------------------------------------------------------
// Runtime Supervisor
// ---------------------------------------------------------------------------

// supervisor coordinates the runtime actor subtree.
type supervisor[P Syncable, M any] struct {
	act.Supervisor
	opts                 SupervisorOptions
	adapter              *Adapter[P]
	lifecycle            SupervisorLifecycle
	transition           SupervisorTransitionPhase
	liveStatus           SupervisorStatus
	projectionSpec       snapshot.ProjectionSpec[M]
	events               RuntimeEvents
	statusToken          gen.Ref
	reconciler           reconcilerActorState
	catalog              catalogActorState
	snapshot             snapshot.SupervisorState
	projection           snapshot.ProjectionActorState
	desiredState         MessageApplyCatalogDesiredState
	pendingDesiredState  MessageApplyCatalogDesiredState
	inFlightCalls        map[uint64]runtimeCall
	drainWaiters         []runtimeDrainWaiter
	transitionGeneration int64
}

// ---------------------------------------------------------------------------
// Runtime Messages
// ---------------------------------------------------------------------------

// MessageProposeDesiredState offers a fully resolved state for drain and promotion.
type MessageProposeDesiredState struct {
	desired MessageApplyCatalogDesiredState
}

// MessageSubmitInvocation requests execution of a plugin invocation.
type MessageSubmitInvocation[T Syncable] struct {
	callID               uint64
	context              context.Context
	cancel               context.CancelFunc
	pluginID, rolloutKey string
	expectedGeneration   int64
	fn                   func(context.Context, T) error
	shadow               bool
	result               *runtime.AsyncResult
}

// ---------------------------------------------------------------------------
// Supervisor Initialization
// ---------------------------------------------------------------------------

// newRuntimeSupervisor creates a runtime supervisor process behavior.
func newRuntimeSupervisor[P Syncable, M any](opts SupervisorOptions, adapter *Adapter[P], projectionSpec snapshot.ProjectionSpec[M]) gen.ProcessBehavior {
	return &supervisor[P, M]{
		opts:           opts,
		adapter:        adapter,
		projectionSpec: projectionSpec,
	}
}

// Init validates and initializes the runtime supervisor subtree.
func (s *supervisor[P, M]) Init(...any) (act.SupervisorSpec, error) {
	s.opts = supervisorOptionsWithDefaults(s.opts)
	if s.opts.Name == "" ||
		s.adapter == nil ||
		s.opts.Directory == "" ||
		s.opts.SnapshotReader.ReaderFactory == nil ||
		s.projectionSpec.Parse == nil || s.projectionSpec.Clone == nil || s.projectionSpec.MaxProcs == nil {
		return act.SupervisorSpec{}, fmt.Errorf(
			"actorruntime: name, adapter, reader options, projection, and directory are required",
		)
	}
	if err := s.RegisterName(s.opts.Name); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register runtime supervisor %q: %w", s.opts.Name, err)
	}

	s.events = RuntimeEventsFor(s.Node(), s.opts.Name)
	s.inFlightCalls = make(map[uint64]runtimeCall)
	s.lifecycle = SupervisorStarting
	s.catalog.status = newCatalogStatus(nil)
	s.projection.Retry = runtime.NewScheduledBackoff(s.opts.RetryMin, s.opts.RetryMax)
	s.reconciler.status = reconcilerActorStatus{
		lifecycle:    ReconcilerActorStarting,
		availability: runtime.AvailabilityUnavailable,
	}
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
		Type:                act.SupervisorTypeRestForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyTransient,
			Intensity: supervisorChildRestartIntensity,
			Period:    supervisorChildRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{
			{
				Name: s.snapshotSupervisorName(),
				Factory: func() gen.ProcessBehavior {
					readerOptions := s.opts.SnapshotReader
					readerOptions.Name = s.snapshotSupervisorName()
					return snapshot.NewSupervisor(snapshot.SupervisorOptions[M]{
						ReaderActorOptions: readerOptions,
						Projection:         s.projectionSpec,
						ProjectionMode:     snapshot.ProjectionCommitExternal,
					})
				},
			},
			{
				Name: s.reconcilerActorName(),
				Factory: func() gen.ProcessBehavior {
					events := snapshot.EventsFor(s.Node(), s.snapshotSupervisorName())
					return newReconcilerActor(
						events,
						s.opts.Directory,
						s.opts.RetryMin,
						s.opts.RetryMax,
					)
				},
			},
			{
				Name: s.catalogActorName(),
				Factory: func() gen.ProcessBehavior {
					return newCatalogActor(s.opts.CatalogOptions, s.adapter)
				},
				Options: gen.ProcessOptions{},
			},
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Supervisor Message Handling
// ---------------------------------------------------------------------------

// HandleCall remains control-plane only. Plugin execution enters through
// MessageSubmitInvocation in HandleMessage and never blocks an Ergo Call callback.
func (s *supervisor[P, M]) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	switch request.(type) {
	case DrainRequest:
		s.drainWaiters = append(s.drainWaiters, runtimeDrainWaiter{pid: from, ref: ref})
		if s.lifecycle == SupervisorDraining {
			return nil, nil
		}

		s.lifecycle = SupervisorDraining
		s.cancelProjectionDeadline()
		s.reconcileStatus()
		s.publishStatus()
		if s.catalog.pid != (gen.PID{}) {
			_ = s.Send(s.catalog.pid, MessageDrain{})
		}
		return nil, nil

	case SupervisorStatusRequest:
		return SupervisorStatusResponse{Status: s.liveStatus.clone()}, nil

	case SupervisorStateRequest:
		if !s.supervisorStateReader() {
			return nil, runtime.ErrPluginUnavailable
		}
		return SupervisorStateResponse{Generation: s.projection.ReadyGeneration}, nil

	default:
		return nil, fmt.Errorf("actorruntime: unsupported supervisor call %T", request)
	}
}

// HandleMessage processes runtime control and child-actor messages.
func (s *supervisor[P, M]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageSubmitInvocation[P]:
		if !s.acceptsSubmission(m.expectedGeneration) {
			m.result.Complete(runtime.ErrPluginUnavailable)
			return nil
		}
		if err := m.context.Err(); err != nil {
			m.result.Complete(err)
			return nil
		}

		catalogPID := s.catalog.pid
		s.inFlightCalls[m.callID] = runtimeCall{
			catalog: catalogPID,
			result:  m.result,
			cancel:  m.cancel,
		}
		call := MessageInvokePlugin[P]{
			CallID:     m.callID,
			Context:    m.context,
			Cancel:     m.cancel,
			PluginID:   m.pluginID,
			RolloutKey: m.rolloutKey,
			Fn:         m.fn,
			Shadow:     m.shadow,
		}
		if err := s.Send(catalogPID, call); err != nil {
			s.finishCall(m.callID, runtime.ErrPluginUnavailable)
			_ = s.Node().SendExit(
				catalogPID,
				fmt.Errorf("forward invocation to catalog: %w", err),
			)
		}

	case MessageReconcilerActorStatusChanged:
		if from != s.reconciler.pid {
			return nil
		}
		s.reconciler.status = m.status
		if m.status.snapshotGeneration != s.desiredState.snapshotGeneration ||
			m.status.revision != s.desiredState.desiredRevision ||
			m.status.availability != runtime.AvailabilityReady {
			if s.transition != SupervisorTransitionIdle {
				s.transition = SupervisorTransitionPreparing
			}
		}
		s.completeDesiredStateTransition()
		s.finishDesiredStateTransition()
		s.reconcileStatus()
		s.publishStatus()

	case MessageDesiredStateFreshness:
		if from != s.reconciler.pid ||
			s.transition != SupervisorTransitionAwaitingFreshness ||
			s.pendingDesiredState.desiredRevision != 0 ||
			m.snapshotGeneration != s.transitionGeneration ||
			m.snapshotGeneration != s.desiredState.snapshotGeneration ||
			m.desiredRevision != s.desiredState.desiredRevision {
			return nil
		}
		s.transition = SupervisorTransitionAwaitingProjection
		s.projection.CommittedGeneration = m.snapshotGeneration
		s.projection.ReadyGeneration = 0
		err := s.requestProjectionCommit()
		s.reconcileStatus()
		s.publishStatus()
		return err

	case snapshot.MessageProjectionActorStatusChanged:
		if from != s.snapshot.Pid {
			return nil
		}
		return s.handleProjectionStatus(m.Status, m.ProjectionPID)

	case snapshot.MessageProjectionCommitResult:
		// A NACK is stamped by the authenticated snapshot supervisor with its
		// current projection PID, allowing recovery from a stale pending PID.
		if from != s.snapshot.Pid ||
			m.Generation != s.projection.PendingGeneration ||
			(m.Err == nil && m.ProjectionPID != s.projection.PendingPID) {
			return nil
		}
		return s.handleProjectionCommitResult(m)

	case MessageProjectionCommitRetry:
		if !s.projection.Retry.Pending || s.projection.Retry.Token != m.token {
			return nil
		}
		s.projection.Retry.Pending = false
		s.projection.Retry.Cancel = nil
		s.projection.PendingGeneration = 0
		s.projection.PendingPID = gen.PID{}
		return s.requestProjectionCommit()

	case MessageProjectionCommitDeadline:
		if m.token != s.projection.DeadlineToken ||
			m.generation != s.projection.PendingGeneration ||
			m.projectionPID != s.projection.PendingPID {
			return nil
		}
		s.projection.DeadlineCancel = nil
		s.projection.PendingGeneration = 0
		s.projection.PendingPID = gen.PID{}
		return s.scheduleProjectionCommitRetry()

	case MessageProposeDesiredState:
		if from != s.reconciler.pid ||
			s.lifecycle == SupervisorDraining ||
			m.desired.desiredRevision <= s.desiredState.desiredRevision ||
			m.desired.desiredRevision <= s.pendingDesiredState.desiredRevision ||
			m.desired.snapshotGeneration < s.desiredState.snapshotGeneration ||
			m.desired.snapshotGeneration < s.pendingDesiredState.snapshotGeneration {
			return nil
		}

		s.pendingDesiredState = m.desired
		if !s.pendingProjectionReady() {
			s.reconcileStatus()
			s.publishStatus()
			return nil
		}
		return s.beginPendingDesiredStateTransition()

	case MessageCancelInvocation:
		call, ok := s.inFlightCalls[m.CallID]
		if !ok {
			return nil
		}
		if call.cancel != nil {
			call.cancel()
		}
		if err := s.SendWithPriority(call.catalog, m, gen.MessagePriorityHigh); err != nil {
			s.finishCall(m.CallID, m.Err)
		}

	case MessageInvocationCompleted:
		if call, ok := s.inFlightCalls[m.CallID]; ok && from == call.catalog {
			s.finishCall(m.CallID, m.Err)
		}

	case MessageCatalogStatusChanged:
		if from != s.catalog.pid ||
			m.pid != s.catalog.pid ||
			m.epoch <= s.catalog.lastEpoch {
			return nil
		}

		s.catalog.lastEpoch = m.epoch
		s.mergeCatalogStatus(m.status)
		s.completeDesiredStateTransition()
		s.finishDesiredStateTransition()
		s.reconcileStatus()
		s.publishStatus()

	case MessageCatalogDrained:
		if s.lifecycle != SupervisorDraining ||
			from != s.catalog.pid ||
			m.pid != s.catalog.pid {
			return nil
		}

		for callID := range s.inFlightCalls {
			s.finishCall(callID, runtime.ErrPluginUnavailable)
		}
		for _, waiter := range s.drainWaiters {
			if waiter.alive() {
				_ = s.SendResponse(waiter.pid, waiter.ref, DrainResponse{})
			}
		}
		s.drainWaiters = nil
		return gen.TerminateReasonNormal
	}
	return nil
}

// ---------------------------------------------------------------------------
// Child Lifecycle
// ---------------------------------------------------------------------------

// HandleChildStart records a child actor incarnation.
func (s *supervisor[P, M]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	switch name {
	case s.snapshotSupervisorName():
		s.startSnapshotSupervisor(pid)

	case s.reconcilerActorName():
		return s.startReconcilerActor(pid)

	case s.catalogActorName():
		return s.startCatalogActor(pid)
	}
	return nil
}

// HandleChildTerminate retires a terminated child actor incarnation.
func (s *supervisor[P, M]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	switch name {
	case s.snapshotSupervisorName():
		if s.snapshot.Pid == pid {
			s.snapshot.Pid = gen.PID{}
			s.projection.ReadyGeneration = 0
		}

	case s.reconcilerActorName():
		s.retireReconcilerActor(pid)

	case s.catalogActorName():
		s.retireCatalogActor(pid, reason)
	}

	s.reconcileStatus()
	s.publishStatus()
	return nil
}

// Terminate stops the runtime supervisor and completes outstanding calls.
func (s *supervisor[P, M]) Terminate(reason error) {
	s.lifecycle = SupervisorDraining
	s.cancelProjectionCommitRetry(false)
	s.cancelProjectionDeadline()
	for callID := range s.inFlightCalls {
		s.finishCall(callID, runtime.ErrPluginUnavailable)
	}
}

// ---------------------------------------------------------------------------
// Component Lifecycle
// ---------------------------------------------------------------------------

// startSnapshotSupervisor records a new snapshot supervisor incarnation.
func (s *supervisor[P, M]) startSnapshotSupervisor(pid gen.PID) {
	if s.snapshot.Pid == pid {
		return
	}
	s.snapshot.Pid = pid
	s.snapshot.Epoch++
	s.projection.Pid = gen.PID{}
	s.projection.ReadyGeneration = 0
	s.projection.PendingGeneration = 0
	s.projection.PendingPID = gen.PID{}
	s.projection.Status = snapshot.ProjectionActorStatus{
		Lifecycle:    snapshot.ProjectionActorRestarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	s.cancelProjectionCommitRetry(false)
	s.cancelProjectionDeadline()
	s.reconcileStatus()
	s.publishStatus()
}

// startReconcilerActor initializes a desired-state reconciler incarnation.
func (s *supervisor[P, M]) startReconcilerActor(pid gen.PID) error {
	state := &s.reconciler
	if s.transition != SupervisorTransitionIdle {
		s.transition = SupervisorTransitionPreparing
	}
	state.pid = pid
	state.status = reconcilerActorStatus{
		lifecycle:    ReconcilerActorStarting,
		availability: runtime.AvailabilityUnavailable,
	}
	s.reconcileStatus()
	s.publishStatus()

	revisionBase := s.desiredState.desiredRevision
	if s.pendingDesiredState.desiredRevision > revisionBase {
		revisionBase = s.pendingDesiredState.desiredRevision
	}
	if err := s.Send(pid, MessageReconcilerActorActivate{revisionBase: revisionBase}); err != nil {
		_ = s.Node().SendExit(
			pid,
			fmt.Errorf("activate desired-state reconciler: %w", err),
		)
	}
	return nil
}

// startCatalogActor initializes a catalog actor incarnation.
func (s *supervisor[P, M]) startCatalogActor(pid gen.PID) error {
	state := &s.catalog
	state.pid = pid
	state.lastEpoch = 0
	state.status = newCatalogStatus(state.status.lastError)
	s.reconcileStatus()
	s.publishStatus()

	if err := s.Send(pid, MessageCatalogActivate{}); err != nil {
		_ = s.Node().SendExit(pid, fmt.Errorf("activate catalog: %w", err))
		return nil
	}
	if s.desiredState.desiredRevision != 0 {
		if err := s.Send(pid, s.desiredState); err != nil {
			_ = s.Node().SendExit(pid, fmt.Errorf("replay desired state to catalog: %w", err))
			return nil
		}
	}
	if s.lifecycle == SupervisorDraining {
		if err := s.Send(pid, MessageDrain{}); err != nil {
			_ = s.Node().SendExit(
				pid,
				fmt.Errorf("drain replacement catalog: %w", err),
			)
		}
	}
	return nil
}

// retireReconcilerActor marks a reconciler incarnation unavailable.
func (s *supervisor[P, M]) retireReconcilerActor(pid gen.PID) {
	state := &s.reconciler
	if state.pid != pid {
		return
	}

	state.pid = gen.PID{}
	if s.transition != SupervisorTransitionIdle {
		s.transition = SupervisorTransitionPreparing
	}
	state.status.lifecycle = ReconcilerActorRestarting
	state.status.availability = runtime.AvailabilityUnavailable
}

// retireCatalogActor marks a catalog incarnation unavailable.
func (s *supervisor[P, M]) retireCatalogActor(pid gen.PID, reason error) {
	state := &s.catalog
	if state.pid != pid {
		return
	}

	state.pid = gen.PID{}
	state.lastEpoch = 0
	state.status.lifecycle = CatalogActorRestarting
	state.status.availability = runtime.AvailabilityUnavailable
	state.status.lastError = reason

	for callID, call := range s.inFlightCalls {
		if call.catalog == pid {
			s.finishCall(callID, runtime.ErrPluginUnavailable)
		}
	}
}

// ---------------------------------------------------------------------------
// Invocation Lifecycle
// ---------------------------------------------------------------------------

// finishCall completes and removes an in-flight plugin invocation.
func (s *supervisor[P, M]) finishCall(callID uint64, err error) {
	call, ok := s.inFlightCalls[callID]
	if !ok {
		return
	}
	delete(s.inFlightCalls, callID)
	if call.cancel != nil {
		call.cancel()
	}
	call.result.Complete(err)
	_ = s.promotePendingDesiredState()
}

// ---------------------------------------------------------------------------
// Desired State Transitions
// ---------------------------------------------------------------------------

// promotePendingDesiredState applies the latest proposal after tracked calls drain.
func (s *supervisor[P, M]) promotePendingDesiredState() error {
	if s.lifecycle == SupervisorDraining || s.transition == SupervisorTransitionIdle || len(s.inFlightCalls) != 0 ||
		s.pendingDesiredState.desiredRevision == 0 ||
		s.pendingDesiredState.snapshotGeneration != s.transitionGeneration {
		return nil
	}
	s.desiredState = s.pendingDesiredState
	s.pendingDesiredState = MessageApplyCatalogDesiredState{}
	if s.catalog.pid != (gen.PID{}) {
		if err := s.Send(s.catalog.pid, s.desiredState); err != nil {
			_ = s.Node().SendExit(
				s.catalog.pid,
				fmt.Errorf("apply desired state to catalog: %w", err),
			)
		}
	}
	s.completeDesiredStateTransition()
	s.reconcileStatus()
	s.publishStatus()
	return nil
}

// beginPendingDesiredStateTransition closes admission only after the target projection is prepared.
func (s *supervisor[P, M]) beginPendingDesiredStateTransition() error {
	if s.lifecycle == SupervisorDraining || s.pendingDesiredState.desiredRevision == 0 || !s.pendingProjectionReady() {
		return nil
	}

	s.transition = SupervisorTransitionPreparing
	s.transitionGeneration = s.pendingDesiredState.snapshotGeneration
	s.cancelProjectionCommitRetry(false)
	s.cancelProjectionDeadline()
	s.projection.PendingGeneration = 0
	s.projection.PendingPID = gen.PID{}
	s.reconcileStatus()
	s.publishStatus()
	return s.promotePendingDesiredState()
}

// pendingProjectionReady reports whether the pending projection can transition.
func (s *supervisor[P, M]) pendingProjectionReady() bool {
	target := s.pendingDesiredState.snapshotGeneration
	if target == 0 {
		return false
	}
	if target == s.projection.CommittedGeneration {
		return s.projection.ReadyGeneration == target &&
			s.projection.Status.Lifecycle == snapshot.ProjectionActorRunning &&
			s.projection.Status.Availability == runtime.AvailabilityReady &&
			s.projection.Status.CommittedGeneration == target &&
			s.projection.Status.PreparedGeneration == 0
	}
	return s.projection.Status.Lifecycle == snapshot.ProjectionActorRunning &&
		s.projection.Status.Availability == runtime.AvailabilityReady &&
		s.projection.Status.PreparedGeneration == target
}

// completeDesiredStateTransition requests a freshness confirmation when ready.
func (s *supervisor[P, M]) completeDesiredStateTransition() {
	if !s.desiredStateTransitionReadyToCommit() {
		return
	}
	if err := s.Send(s.reconciler.pid, MessageDesiredStateFreshness{
		snapshotGeneration: s.transitionGeneration,
		desiredRevision:    s.desiredState.desiredRevision,
	}); err == nil {
		s.transition = SupervisorTransitionAwaitingFreshness
	}
}

// desiredStateTransitionReadyToCommit reports whether all transition dependencies converged.
func (s *supervisor[P, M]) desiredStateTransitionReadyToCommit() bool {
	return s.transition == SupervisorTransitionPreparing &&
		s.pendingDesiredState.desiredRevision == 0 &&
		s.reconciler.status.snapshotGeneration == s.transitionGeneration &&
		s.reconciler.status.availability == runtime.AvailabilityReady &&
		s.desiredState.desiredRevision != 0 &&
		s.desiredState.snapshotGeneration == s.transitionGeneration &&
		s.reconciler.status.revision == s.desiredState.desiredRevision &&
		s.catalog.status.desiredRevision == s.desiredState.desiredRevision &&
		s.catalog.status.availability == runtime.AvailabilityReady
}

// finishDesiredStateTransition reopens admission after every current dependency converges.
func (s *supervisor[P, M]) finishDesiredStateTransition() {
	if s.transition != SupervisorTransitionAwaitingProjection ||
		s.pendingDesiredState.desiredRevision != 0 ||
		s.projection.ReadyGeneration != s.transitionGeneration ||
		s.projection.CommittedGeneration != s.transitionGeneration ||
		s.projection.Status.Lifecycle != snapshot.ProjectionActorRunning ||
		!s.projection.Status.Availability.Routable() ||
		s.projection.Status.CommittedGeneration != s.transitionGeneration ||
		s.desiredState.snapshotGeneration != s.transitionGeneration ||
		s.reconciler.status.snapshotGeneration != s.transitionGeneration ||
		s.reconciler.status.revision != s.desiredState.desiredRevision ||
		s.reconciler.status.availability != runtime.AvailabilityReady ||
		s.catalog.status.desiredRevision != s.desiredState.desiredRevision ||
		s.catalog.status.availability != runtime.AvailabilityReady {
		return
	}
	s.transition = SupervisorTransitionIdle
}

// ---------------------------------------------------------------------------
// Status Reporting
// ---------------------------------------------------------------------------
// newCatalogStatus returns an initial catalog status for an actor incarnation.
func newCatalogStatus(lastError error) catalogActorStatus {
	return catalogActorStatus{
		lifecycle:    CatalogActorStarting,
		availability: runtime.AvailabilityUnavailable,
		lastError:    lastError,
		routers:      make(map[string]routerActorStatus),
	}
}

// mergeCatalogStatus merges the latest catalog status into supervisor state.
func (s *supervisor[P, M]) mergeCatalogStatus(status catalogActorStatus) {
	state := &s.catalog
	next := status.clone()
	if next.lifecycle == CatalogActorRunning {
		next.lastError = nil
		if s.lifecycle != SupervisorDraining {
			s.lifecycle = SupervisorRunning
		}
	} else {
		next.lastError = state.status.lastError
	}
	state.status = next
}

// reconcileStatus rebuilds the published runtime status.
func (s *supervisor[P, M]) reconcileStatus() {
	lifecycle := s.lifecycle
	if lifecycle == "" {
		lifecycle = SupervisorStarting
	}
	s.liveStatus = SupervisorStatus{
		Lifecycle:       lifecycle,
		Availability:    s.runtimeAvailability(),
		DesiredRevision: s.currentDesiredRevision(),
		Transition:      s.transition,
		Catalog:         s.catalog.status.clone(),
		Reconciler:      s.reconciler.status,
	}
}

// publishStatus emits the current runtime status event.
func (s *supervisor[P, M]) publishStatus() {
	if s.events.Status.Name == "" || s.statusToken == (gen.Ref{}) {
		return
	}
	_ = s.SendEvent(s.events.Status.Name, s.statusToken, s.liveStatus.clone())
}

// runtimeAvailability derives runtime availability from child component status.
func (s *supervisor[P, M]) runtimeAvailability() runtime.Availability {
	if s.lifecycle == SupervisorDraining || s.transition != SupervisorTransitionIdle || !s.projectionReady() ||
		s.projection.Status.Availability == runtime.AvailabilityUnavailable ||
		s.catalog.status.availability == runtime.AvailabilityUnavailable {
		return runtime.AvailabilityUnavailable
	}
	if s.projection.Status.Availability != runtime.AvailabilityReady ||
		s.catalog.status.availability != runtime.AvailabilityReady ||
		s.reconciler.status.availability != runtime.AvailabilityReady {
		return runtime.AvailabilityDegraded
	}
	return runtime.AvailabilityReady
}

// projectionReady reports whether the committed projection generation is ready.
func (s *supervisor[P, M]) projectionReady() bool {
	return s.projection.CommittedGeneration == 0 || s.projection.ReadyGeneration == s.projection.CommittedGeneration
}

// currentDesiredRevision reports the newest applied or pending revision.
func (s *supervisor[P, M]) currentDesiredRevision() uint64 {
	if s.pendingDesiredState.desiredRevision > s.desiredState.desiredRevision {
		return s.pendingDesiredState.desiredRevision
	}
	return s.desiredState.desiredRevision
}

// ---------------------------------------------------------------------------
// Runtime Admission
// ---------------------------------------------------------------------------

// acceptsSubmission reports whether the runtime can accept an invocation.
func (s *supervisor[P, M]) acceptsSubmission(expectedGeneration int64) bool {
	return expectedGeneration > 0 &&
		expectedGeneration == s.projection.ReadyGeneration &&
		expectedGeneration == s.projection.CommittedGeneration &&
		s.projection.Status.Lifecycle == snapshot.ProjectionActorRunning &&
		s.projection.Status.Availability.Routable() &&
		s.projection.Status.CommittedGeneration == expectedGeneration &&
		expectedGeneration == s.desiredState.snapshotGeneration &&
		s.catalog.status.desiredRevision == s.desiredState.desiredRevision &&
		s.lifecycle != SupervisorDraining && s.transition == SupervisorTransitionIdle && s.projectionReady() &&
		s.catalog.pid != (gen.PID{})
}

// ---------------------------------------------------------------------------
// Projection Commit Coordination
// ---------------------------------------------------------------------------

// adoptAuthoritativeProjectionPID updates the active projection PID when it changes.
func (s *supervisor[P, M]) adoptAuthoritativeProjectionPID(pid gen.PID) bool {
	if pid == (gen.PID{}) || pid == s.projection.Pid {
		return false
	}
	s.projection.Pid = pid
	s.projection.ReadyGeneration = 0
	s.projection.PendingGeneration = 0
	s.projection.PendingPID = gen.PID{}
	return true
}

// handleProjectionStatus applies the latest projection actor status.
func (s *supervisor[P, M]) handleProjectionStatus(status snapshot.ProjectionActorStatus, pid gen.PID) error {
	pidChanged := s.adoptAuthoritativeProjectionPID(pid)
	if pidChanged {
		s.cancelProjectionCommitRetry(false)
		s.cancelProjectionDeadline()
	}
	if !pidChanged && s.projection.PendingGeneration == s.projection.CommittedGeneration &&
		s.projection.PendingPID == pid {
		// Status precedes the matching commit result from the child.
		// Keep that correlation alive; only the acknowledgement or deadline resolves it.
		s.projection.ReadyGeneration = 0
		s.projection.Status = status
		s.reconcileStatus()
		s.publishStatus()
		return s.beginPendingDesiredStateTransition()
	}
	if !pidChanged && status.Lifecycle == snapshot.ProjectionActorRunning &&
		status.Availability.Routable() &&
		status.CommittedGeneration == s.projection.CommittedGeneration &&
		s.projection.ReadyGeneration == s.projection.CommittedGeneration {
		s.projection.Status = status
		s.finishDesiredStateTransition()
		s.reconcileStatus()
		s.publishStatus()
		return s.beginPendingDesiredStateTransition()
	}
	s.projection.Status = status

	canActivate := status.Lifecycle == snapshot.ProjectionActorRunning &&
		status.PreparedGeneration == s.projection.CommittedGeneration
	s.projection.ReadyGeneration = 0
	s.projection.PendingGeneration = 0
	s.projection.PendingPID = gen.PID{}
	s.cancelProjectionCommitRetry(false)
	s.cancelProjectionDeadline()
	s.reconcileStatus()
	s.publishStatus()
	if err := s.beginPendingDesiredStateTransition(); err != nil {
		return err
	}
	if canActivate {
		return s.requestProjectionCommit()
	}
	return nil
}

// handleProjectionCommitResult processes a projection commit acknowledgement.
func (s *supervisor[P, M]) handleProjectionCommitResult(m snapshot.MessageProjectionCommitResult) error {
	if m.Err != nil {
		if s.adoptAuthoritativeProjectionPID(m.ProjectionPID) {
			s.cancelProjectionDeadline()
			s.cancelProjectionCommitRetry(false)
			s.reconcileStatus()
			s.publishStatus()
			return s.requestProjectionCommit()
		}
		if m.ProjectionPID != s.projection.PendingPID || m.ProjectionPID != s.projection.Pid {
			return nil
		}
		if s.projection.ReadyGeneration == m.Generation {
			return nil
		}
		s.cancelProjectionDeadline()
		s.projection.PendingGeneration = 0
		s.projection.PendingPID = gen.PID{}
		return s.scheduleProjectionCommitRetry()
	}
	if m.ProjectionPID != s.projection.PendingPID || m.ProjectionPID != s.projection.Pid {
		return nil
	}
	s.cancelProjectionDeadline()
	s.projection.ReadyGeneration = m.Generation
	s.projection.PendingGeneration = 0
	s.projection.PendingPID = gen.PID{}
	s.cancelProjectionCommitRetry(true)
	s.finishDesiredStateTransition()
	if err := s.beginPendingDesiredStateTransition(); err != nil {
		return err
	}
	s.reconcileStatus()
	s.publishStatus()
	return nil
}

// requestProjectionCommit asks the snapshot supervisor to commit the projection.
func (s *supervisor[P, M]) requestProjectionCommit() error {
	if s.Process == nil || s.lifecycle == SupervisorDraining || s.snapshot.Pid == (gen.PID{}) || s.projection.CommittedGeneration == 0 ||
		s.projection.Retry.Pending ||
		(s.projection.PendingGeneration != 0 &&
			(s.projection.PendingGeneration != s.projection.CommittedGeneration ||
				s.projection.PendingPID != s.projection.Pid)) {
		return nil
	}
	if s.projection.PendingGeneration == 0 {
		s.projection.PendingGeneration = s.projection.CommittedGeneration
		s.projection.PendingPID = s.projection.Pid
	}
	if err := s.Send(s.snapshot.Pid, snapshot.MessageProjectionCommit{
		Generation:    s.projection.PendingGeneration,
		ProjectionPID: s.projection.PendingPID,
	}); err != nil {
		return s.scheduleProjectionCommitRetry()
	}
	if err := s.scheduleProjectionDeadline(); err != nil {
		s.projection.PendingGeneration = 0
		s.projection.PendingPID = gen.PID{}
		return s.scheduleProjectionCommitRetry()
	}
	return nil
}

// scheduleProjectionCommitRetry schedules another projection commit attempt.
func (s *supervisor[P, M]) scheduleProjectionCommitRetry() error {
	if s.lifecycle == SupervisorDraining || s.projection.Retry.Pending || s.projection.CommittedGeneration == 0 {
		return nil
	}
	delay := s.projection.Retry.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("projection commit retry: %w", runtime.ErrBackoffStopped)
	}
	if delay <= 0 {
		delay = time.Nanosecond
	}
	s.projection.Retry.Token++
	token := s.projection.Retry.Token
	cancel, err := s.SendAfter(s.PID(), MessageProjectionCommitRetry{token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule projection commit retry: %w", err)
	}
	s.projection.Retry.Pending = true
	s.projection.Retry.Cancel = cancel
	return nil
}

// cancelProjectionCommitRetry cancels any scheduled projection commit retry.
func (s *supervisor[P, M]) cancelProjectionCommitRetry(reset bool) {
	if s.projection.Retry != nil {
		s.projection.Retry.CancelScheduled(reset)
	}
}

// scheduleProjectionDeadline schedules a deadline for the pending projection commit.
func (s *supervisor[P, M]) scheduleProjectionDeadline() error {
	delay := s.opts.ControlTimeout
	s.cancelProjectionDeadline()
	s.projection.DeadlineToken++
	token := s.projection.DeadlineToken
	cancel, err := s.SendAfter(s.PID(), MessageProjectionCommitDeadline{
		token:         token,
		generation:    s.projection.PendingGeneration,
		projectionPID: s.projection.PendingPID,
	}, delay)
	if err == nil {
		s.projection.DeadlineCancel = cancel
	}
	return err
}

// cancelProjectionDeadline cancels the pending projection commit deadline.
func (s *supervisor[P, M]) cancelProjectionDeadline() {
	if s.projection.DeadlineCancel != nil {
		s.projection.DeadlineCancel()
		s.projection.DeadlineCancel = nil
	}
	s.projection.DeadlineToken++
}

// ---------------------------------------------------------------------------
// Runtime Helpers
// ---------------------------------------------------------------------------

// reconcilerActorName returns the desired-state reconciler actor name.
func (s *supervisor[P, M]) reconcilerActorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-desired-state-reconciler")
}

// snapshotSupervisorName returns the snapshot supervisor name.
func (s *supervisor[P, M]) snapshotSupervisorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-snapshot")
}

// catalogActorName returns the catalog actor name.
func (s *supervisor[P, M]) catalogActorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-catalog")
}

// supervisorStateReader reports whether the runtime state is ready for callers.
func (s *supervisor[P, M]) supervisorStateReader() bool {
	return s.projection.ReadyGeneration != 0 &&
		s.projection.ReadyGeneration == s.projection.CommittedGeneration &&
		s.projection.Status.CommittedGeneration == s.projection.ReadyGeneration &&
		s.projection.Status.Availability.Routable() &&
		s.desiredState.snapshotGeneration == s.projection.ReadyGeneration &&
		s.catalog.status.desiredRevision == s.desiredState.desiredRevision &&
		s.catalog.status.availability == runtime.AvailabilityReady &&
		s.transition == SupervisorTransitionIdle && s.lifecycle != SupervisorDraining
}

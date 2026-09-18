package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

const (
	supervisorChildRestartIntensity uint16 = 5
	supervisorChildRestartPeriod    uint16 = 5
)

// SupervisorLifecycle is what the runtime subtree does with invocations: Draining and Stopped take none.
type SupervisorLifecycle string

const (
	SupervisorStarting SupervisorLifecycle = "starting"
	SupervisorRunning  SupervisorLifecycle = "running"
	SupervisorDraining SupervisorLifecycle = "draining"
	SupervisorStopped  SupervisorLifecycle = "stopped"
)

// SupervisorTransitionPhase describes desired-state transition progress.
type SupervisorTransitionPhase uint8

const (
	SupervisorTransitionIdle SupervisorTransitionPhase = iota
	SupervisorTransitionPreparing
	SupervisorTransitionAwaitingFreshness
	SupervisorTransitionAwaitingProjection
)

// String names the phase for inspection; the gauge publishes the ordinal instead.
func (p SupervisorTransitionPhase) String() string {
	switch p {
	case SupervisorTransitionPreparing:
		return "preparing"
	case SupervisorTransitionAwaitingFreshness:
		return "awaiting_freshness"
	case SupervisorTransitionAwaitingProjection:
		return "awaiting_projection"
	default:
		return "idle"
	}
}

// SupervisorStatus is the authoritative public status the runtime supervisor publishes.
type SupervisorStatus struct {
	Lifecycle       SupervisorLifecycle
	Availability    runtime.Availability
	DesiredRevision uint64
	Transition      SupervisorTransitionPhase
	Catalog         catalogActorStatus
	Reconciler      reconcilerActorStatus
	err             error
}

// clone returns an independent copy of the runtime status.
func (s SupervisorStatus) clone() SupervisorStatus {
	clone := s
	clone.Catalog = s.Catalog.clone()
	return clone
}

// runtimeDrainWaiter tracks a caller waiting for runtime drain completion.
type runtimeDrainWaiter struct {
	pid gen.PID
	ref gen.Ref
}

// alive reports whether the drain waiter can still receive a response.
func (t runtimeDrainWaiter) alive() bool { return t.ref.IsAlive() }

// runtimeCall tracks an in-flight runtime call. A completed call stays tracked until the tree reports
// its plugin capacity released, so the gateway that owns its admission permit hears both facts.
type runtimeCall struct {
	ref       InvocationRef
	owner     gen.PID
	catalog   gen.PID
	result    *runtime.AsyncResult
	cancel    context.CancelFunc
	completed bool
}

// supervisor coordinates the runtime actor subtree.
type supervisor[P Artifact, M any] struct {
	act.Supervisor
	opts                      SupervisorOptions
	namespace                 string
	adapter                   *Adapter[P]
	loader                    snapshot.Loader[M]
	reconciler                reconcilerActorState
	catalog                   catalogActorState
	snapshot                  snapshot.SupervisorState
	projection                snapshot.ProjectionActorState
	lastProjectionStatusEpoch int64
	desiredState              MessageApplyCatalogDesiredState
	pendingDesiredState       MessageApplyCatalogDesiredState
	inFlightCalls             map[uint64]runtimeCall
	drainWaiters              []runtimeDrainWaiter
	transitionGeneration      int64
	collectorsRegistered      bool
	radarLogged               bool
	labels                    telemetry.Labels
	signal                    telemetry.Signal
	lifecycle                 SupervisorLifecycle
	transition                SupervisorTransitionPhase
	err                       error            // the supervisor's own failure, kept apart from its children's errors
	lastStatus                SupervisorStatus // last reconciled projection; queries read live state instead
}

// ---------------------------------------------------------------------------
// Messages
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

// MessageRadarTick drives the supervisor's periodic radar reconcile.
type MessageRadarTick struct{}

// MessageProjectionCommitRetry triggers a deferred projection commit retry.
type MessageProjectionCommitRetry struct{ token uint64 }

// MessageProjectionCommitDeadline marks a projection commit attempt as expired.
type MessageProjectionCommitDeadline struct {
	token         uint64
	generation    int64
	projectionPID gen.PID
}

// MessageProposeDesiredState offers a fully resolved state for drain and promotion.
type MessageProposeDesiredState struct {
	desired MessageApplyCatalogDesiredState
}

// MessageSubmitInvocation requests execution of a plugin invocation the gateway has admitted.
type MessageSubmitInvocation[T Artifact] struct {
	ref                  InvocationRef
	context              context.Context
	cancel               context.CancelFunc
	pluginID, rolloutKey string
	expectedGeneration   int64
	fn                   func(context.Context, T) error
	shadow               bool
	result               *runtime.AsyncResult
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// newRuntimeSupervisor creates a runtime supervisor process behavior.
func newRuntimeSupervisor[P Artifact, M any](namespace string, opts SupervisorOptions, adapter *Adapter[P], loader snapshot.Loader[M]) gen.ProcessBehavior {
	return &supervisor[P, M]{
		namespace: namespace,
		opts:      opts,
		adapter:   adapter,
		loader:    loader,
		labels:    telemetry.NewLabels(namespace),
		signal:    newHealthSignal(namespace),
	}
}

// Init validates and initializes the runtime supervisor subtree.
func (s *supervisor[P, M]) Init(...any) (act.SupervisorSpec, error) {
	s.opts = supervisorOptionsWithDefaults(s.opts)
	// Namespace is required: every process name in this subtree, and every metric label, comes from it.
	if s.namespace == "" ||
		s.adapter == nil ||
		s.opts.Directory == "" ||
		s.opts.SnapshotReader.Endpoint.Name == "" ||
		s.opts.SnapshotReader.ExecutorID == "" || s.loader == nil {
		return act.SupervisorSpec{}, fmt.Errorf(
			"namespace, adapter, reader options, projection, and directory are required",
		)
	}
	if err := requireSubtreeName(s, SupervisorName(s.namespace)); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("runtime supervisor name: %w", err)
	}

	s.inFlightCalls = make(map[uint64]runtimeCall)
	s.lifecycle = SupervisorStarting
	s.catalog.status = newCatalogStatus(nil)
	s.projection.Retry = runtime.NewScheduledBackoff(s.opts.RetryMin, s.opts.RetryMax)
	s.reconciler.status = reconcilerActorStatus{
		lifecycle:    ReconcilerActorStarting,
		availability: runtime.AvailabilityUnavailable,
	}
	s.reconcileStatus()
	// A message, not an inline call: radar must not delay the spec.
	if err := s.Send(s.PID(), MessageRadarTick{}); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("schedule radar tick: %w", err)
	}

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
					return snapshot.NewSupervisor(snapshot.SupervisorOptions{
						Namespace:          s.namespace,
						ReaderActorOptions: s.opts.SnapshotReader,
						ProjectionMode:     snapshot.ProjectionCommitExternal,
					}, s.loader)
				},
			},
			{
				Name: s.reconcilerActorName(),
				Factory: func() gen.ProcessBehavior {
					return newReconcilerActor(
						snapshot.ArtifactsEventFor(s.Node(), s.namespace),
						snapshot.ReaderActorStatusEventFor(s.Node(), s.namespace),
						s.opts.Directory,
						s.opts.RetryMin,
						s.opts.RetryMax,
						s.opts.RestartMin,
						s.opts.RestartMax,
						s.labels,
					)
				},
			},
			{
				Name: s.catalogActorName(),
				Factory: func() gen.ProcessBehavior {
					return newCatalogActor(s.opts.CatalogOptions, s.adapter, s.labels)
				},
			},
		},
	}, nil
}

// HandleCall is control-plane only; execution enters through MessageSubmitInvocation instead.
func (s *supervisor[P, M]) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	defer s.reconcileStatus()
	switch request.(type) {
	case DrainRequest:
		if s.lifecycle == SupervisorStopped {
			return DrainResponse{Err: ErrRuntimeStopped}, nil
		}
		s.drainWaiters = append(s.drainWaiters, runtimeDrainWaiter{pid: from, ref: ref})
		if s.lifecycle == SupervisorDraining {
			return nil, nil
		}

		s.lifecycle = SupervisorDraining
		s.cancelProjectionDeadline()
		if s.catalog.pid != (gen.PID{}) {
			// Keep the drain behind invocations already forwarded to the catalog.
			_ = s.Send(s.catalog.pid, MessageDrain{})
		}
		return nil, nil

	case SupervisorStatusRequest:
		return SupervisorStatusResponse{Status: s.status().clone()}, nil

	case SupervisorStateRequest:
		if !s.supervisorStateReader() {
			return SupervisorStateResponse{}, nil
		}
		return SupervisorStateResponse{Generation: s.projection.ReadyGeneration}, nil

	default:
		return nil, fmt.Errorf("unsupported supervisor call %T", request)
	}
}

// HandleMessage processes runtime control and child-actor messages.
func (s *supervisor[P, M]) HandleMessage(from gen.PID, message any) error {
	defer s.reconcileStatus()
	switch m := message.(type) {
	case MessageRadarTick:
		if from != s.PID() {
			return nil
		}
		s.reconcileRadar()
		if _, err := s.SendAfter(s.PID(), MessageRadarTick{}, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("reschedule radar tick: %w", err)
		}
		return nil

	case gen.MessageDownProcessID:
		// Forget what a restarted radar lost so the next tick registers it again.
		switch m.ProcessID.Name {
		case telemetry.MetricsProcess:
			s.collectorsRegistered = false
		case telemetry.HealthProcess:
			s.signal = newHealthSignal(s.namespace)
		default:
			return nil
		}
		s.Log().Debug("radar process down, re-registering on next tick: namespace=%q process=%s", s.namespace, m.ProcessID.Name)
		return nil

	case MessageSubmitInvocation[P]:
		// The sender has to be the gateway its own reference names, which is the identity notifyOwner
		// requires to answer: anything else holds no admission permit and could hear no result.
		if from != m.ref.Gateway {
			s.labels.Count(s, metricInvocationsRejected, "owner")
			s.rejectSubmission(from, m, ErrPluginUnavailable)
			return nil
		}
		if !s.acceptsSubmission(m.expectedGeneration) {
			s.labels.Count(s, metricInvocationsRejected, "closed")
			s.rejectSubmission(from, m, ErrPluginUnavailable)
			return nil
		}
		if err := m.context.Err(); err != nil {
			s.labels.Count(s, metricInvocationsRejected, "context")
			s.rejectSubmission(from, m, err)
			return nil
		}

		catalogPID := s.catalog.pid
		s.inFlightCalls[m.ref.CallID] = runtimeCall{
			ref:     m.ref,
			owner:   from,
			catalog: catalogPID,
			result:  m.result,
			cancel:  m.cancel,
		}
		call := MessageInvokePlugin[P]{
			CallID:     m.ref.CallID,
			Context:    m.context,
			Cancel:     m.cancel,
			PluginID:   m.pluginID,
			RolloutKey: m.rolloutKey,
			Fn:         m.fn,
			Shadow:     m.shadow,
		}
		if err := s.Send(catalogPID, call); err != nil {
			s.finishCall(m.ref.CallID, ErrPluginUnavailable)
			_ = s.Node().SendExit(
				catalogPID,
				fmt.Errorf("forward invocation to catalog: %w", err),
			)
			return nil
		}
		s.notifyOwner(m.ref, from, MessageGatewayInvocationAdmitted{Ref: m.ref})

	case MessageReconcilerActorStatusChanged:
		if from != s.reconciler.pid || m.epoch <= s.reconciler.statusEpoch {
			return nil
		}
		s.reconciler.statusEpoch = m.epoch
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
		if s.projection.ReadyGeneration == m.snapshotGeneration &&
			s.projection.CommittedGeneration == m.snapshotGeneration {
			s.finishDesiredStateTransition()
			return nil
		}
		s.projection.CommittedGeneration = m.snapshotGeneration
		s.projection.ReadyGeneration = 0
		return s.requestProjectionCommit()

	case snapshot.MessageProjectionActorStatusChanged:
		if from != s.snapshot.Pid || m.StatusEpoch <= s.lastProjectionStatusEpoch {
			return nil
		}
		s.lastProjectionStatusEpoch = m.StatusEpoch
		return s.handleProjectionStatus(m.Status, m.ProjectionPID)

	case snapshot.MessageProjectionCommitResult:
		// A NACK carries the snapshot supervisor's current projection PID, so a stale one recovers.
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
		s.err = fmt.Errorf("projection commit deadline: %w", context.DeadlineExceeded)
		s.projection.DeadlineCancel = nil
		s.projection.PendingGeneration = 0
		s.projection.PendingPID = gen.PID{}
		return s.scheduleProjectionCommitRetry()

	case MessageProposeDesiredState:
		if from != s.reconciler.pid ||
			s.lifecycle == SupervisorDraining ||
			s.lifecycle == SupervisorStopped ||
			m.desired.desiredRevision <= s.desiredState.desiredRevision ||
			m.desired.desiredRevision <= s.pendingDesiredState.desiredRevision ||
			m.desired.snapshotGeneration < s.desiredState.snapshotGeneration ||
			m.desired.snapshotGeneration < s.pendingDesiredState.snapshotGeneration {
			return nil
		}

		s.pendingDesiredState = m.desired
		if !s.pendingProjectionReady() {
			return nil
		}
		return s.beginPendingDesiredStateTransition()

	case MessageGatewayCancelInvocation:
		call, ok := s.inFlightCalls[m.Ref.CallID]
		if !ok || call.ref != m.Ref || from != call.owner {
			return nil
		}
		if call.cancel != nil {
			call.cancel()
		}
		if err := s.SendWithPriority(call.catalog, MessageCancelInvocation{
			CallID: m.Ref.CallID, Err: m.Err,
		}, gen.MessagePriorityHigh); err != nil {
			s.finishCall(m.Ref.CallID, m.Err)
		}

	case MessageInvocationCompleted:
		call, ok := s.inFlightCalls[m.CallID]
		if !ok || from != call.catalog {
			return nil
		}
		// Always this call's first answer: the catalog drops a second completion for a call it already
		// reported, so an executing one is finished by MessageInvocationReleased below instead.
		if !m.Executing {
			s.finishCall(m.CallID, m.Err)
			return nil
		}
		// The caller gets its result now; the plugin capacity behind it is only free once the tree says so,
		// so this call stays tracked and its gateway permit stays held.
		s.completeCall(m.CallID, m.Err)

	case MessageInvocationReleased:
		call, ok := s.inFlightCalls[m.CallID]
		if !ok || from != call.catalog || !call.completed {
			return nil
		}
		s.releaseCall(m.CallID)

	case MessageCatalogStatusChanged:
		if from != s.catalog.pid ||
			m.pid != s.catalog.pid || m.statusEpoch <= s.catalog.statusEpoch {
			return nil
		}
		s.catalog.statusEpoch = m.statusEpoch

		s.mergeCatalogStatus(m.status)
		s.completeDesiredStateTransition()
		s.finishDesiredStateTransition()

	case MessageCatalogDrained:
		if s.lifecycle != SupervisorDraining ||
			from != s.catalog.pid ||
			m.pid != s.catalog.pid {
			return nil
		}

		for callID := range s.inFlightCalls {
			s.finishCall(callID, ErrPluginUnavailable)
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

// HandleChildStart records a child actor incarnation.
func (s *supervisor[P, M]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	defer s.reconcileStatus()
	switch name {
	case s.snapshotSupervisorName():
		s.labels.Count(s, metricChildStarts, "snapshot")
		s.startSnapshotSupervisor(pid)

	case s.reconcilerActorName():
		s.labels.Count(s, metricChildStarts, "reconciler")
		return s.startReconcilerActor(pid)

	case s.catalogActorName():
		s.labels.Count(s, metricChildStarts, "catalog")
		return s.startCatalogActor(pid)
	}
	return nil
}

// HandleChildTerminate retires a terminated child actor incarnation.
func (s *supervisor[P, M]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	defer s.reconcileStatus()
	switch name {
	case s.snapshotSupervisorName():
		s.labels.Count(s, metricChildTerminations, "snapshot", telemetry.TerminationReason(reason))
		if s.snapshot.Pid == pid {
			s.snapshot.Pid = gen.PID{}
			s.projection.ReadyGeneration = 0
			s.projection.Status.Err = reason
		}

	case s.reconcilerActorName():
		s.labels.Count(s, metricChildTerminations, "reconciler", telemetry.TerminationReason(reason))
		if s.reconciler.pid == pid {
			s.retireReconcilerActor(pid)
			s.reconciler.status.err = reason
		}

	case s.catalogActorName():
		s.labels.Count(s, metricChildTerminations, "catalog", telemetry.TerminationReason(reason))
		s.retireCatalogActor(pid, reason)
	}
	return nil
}

// Terminate stops the runtime supervisor and completes outstanding calls.
func (s *supervisor[P, M]) Terminate(reason error) {
	defer s.reconcileStatus()
	s.lifecycle = SupervisorStopped
	s.err = reason
	s.cancelProjectionCommitRetry(false)
	s.cancelProjectionDeadline()
	for callID := range s.inFlightCalls {
		s.finishCall(callID, ErrPluginUnavailable)
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// finishCall completes an in-flight plugin invocation and releases what the tree held for it, which is
// every path but a cancellation the plugin has not finished acting on yet.
func (s *supervisor[P, M]) finishCall(callID uint64, err error) {
	s.completeCall(callID, err)
	s.releaseCall(callID)
}

// completeCall reports one invocation's result to its caller and its gateway, leaving the call tracked.
func (s *supervisor[P, M]) completeCall(callID uint64, err error) {
	call, ok := s.inFlightCalls[callID]
	if !ok || call.completed {
		return
	}
	call.completed = true
	s.inFlightCalls[callID] = call
	if call.cancel != nil {
		call.cancel()
	}
	s.labels.Count(s, metricInvocations, telemetry.Result(err))
	call.result.Complete(err)
	s.notifyOwner(call.ref, call.owner, MessageGatewayInvocationCompleted{Ref: call.ref, Err: err})
	_ = s.promotePendingDesiredState()
}

// releaseCall forgets one invocation and tells its gateway the plugin capacity behind it is free.
func (s *supervisor[P, M]) releaseCall(callID uint64) {
	call, ok := s.inFlightCalls[callID]
	if !ok {
		return
	}
	delete(s.inFlightCalls, callID)
	s.notifyOwner(call.ref, call.owner, MessageGatewayInvocationReleased{Ref: call.ref})
	_ = s.promotePendingDesiredState()
}

// callCounts splits the tracked calls into those whose caller has no result yet and those completed but
// still holding plugin capacity.
func (s *supervisor[P, M]) callCounts() (incomplete, unreleased int) {
	for _, call := range s.inFlightCalls {
		if call.completed {
			unreleased++
			continue
		}
		incomplete++
	}
	return incomplete, unreleased
}

// hasIncompleteCalls reports whether any tracked call still owes its caller a result, which is what a
// revision transition waits on.
func (s *supervisor[P, M]) hasIncompleteCalls() bool {
	for _, call := range s.inFlightCalls {
		if !call.completed {
			return true
		}
	}
	return false
}

// rejectSubmission answers a submission the runtime never took, completing its caller and releasing the
// gateway permit it was admitted against.
func (s *supervisor[P, M]) rejectSubmission(from gen.PID, m MessageSubmitInvocation[P], err error) {
	m.result.Complete(err)
	s.notifyOwner(m.ref, from, MessageGatewayInvocationCompleted{Ref: m.ref, Err: err})
	s.notifyOwner(m.ref, from, MessageGatewayInvocationReleased{Ref: m.ref})
}

// notifyOwner sends one invocation fact to the gateway incarnation that submitted it, and to no other:
// a reused call id belonging to a previous incarnation is addressed to a process that no longer exists.
func (s *supervisor[P, M]) notifyOwner(ref InvocationRef, owner gen.PID, message any) {
	if owner == (gen.PID{}) || ref.Gateway != owner {
		return
	}
	_ = s.SendWithPriority(owner, message, gen.MessagePriorityHigh)
}

// promotePendingDesiredState applies the latest proposal after tracked calls drain.
func (s *supervisor[P, M]) promotePendingDesiredState() error {
	if s.lifecycle == SupervisorDraining ||
		s.lifecycle == SupervisorStopped ||
		s.transition == SupervisorTransitionIdle ||
		s.pendingDesiredState.desiredRevision == 0 ||
		s.pendingDesiredState.snapshotGeneration != s.transitionGeneration ||
		// Last, so the only walk of the tracking map happens on a transition that is otherwise ready.
		s.hasIncompleteCalls() {
		return nil
	}
	s.desiredState = s.pendingDesiredState
	s.pendingDesiredState = MessageApplyCatalogDesiredState{}
	s.labels.Count(s, metricPromotions)
	if s.catalog.pid != (gen.PID{}) {
		if err := s.SendWithPriority(s.catalog.pid, s.desiredState, gen.MessagePriorityHigh); err != nil {
			_ = s.Node().SendExit(
				s.catalog.pid,
				fmt.Errorf("apply desired state to catalog: %w", err),
			)
		}
	}
	s.completeDesiredStateTransition()
	return nil
}

// beginPendingDesiredStateTransition closes admission only after the target projection is prepared.
func (s *supervisor[P, M]) beginPendingDesiredStateTransition() error {
	if s.lifecycle == SupervisorDraining ||
		s.lifecycle == SupervisorStopped ||
		s.pendingDesiredState.desiredRevision == 0 ||
		!s.pendingProjectionReady() {
		return nil
	}

	s.transition = SupervisorTransitionPreparing
	s.transitionGeneration = s.pendingDesiredState.snapshotGeneration
	s.cancelProjectionCommitRetry(false)
	s.cancelProjectionDeadline()
	s.projection.PendingGeneration = 0
	s.projection.PendingPID = gen.PID{}
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
		s.projection.Status.PreparedGeneration == target
}

// completeDesiredStateTransition requests a freshness confirmation when ready.
func (s *supervisor[P, M]) completeDesiredStateTransition() {
	if !s.desiredStateTransitionReadyToCommit() {
		return
	}
	if err := s.SendWithPriority(s.reconciler.pid, MessageDesiredStateFreshness{
		snapshotGeneration: s.transitionGeneration,
		desiredRevision:    s.desiredState.desiredRevision,
	}, gen.MessagePriorityHigh); err == nil {
		s.transition = SupervisorTransitionAwaitingFreshness
	}
}

// desiredStateTransitionReadyToCommit reports whether every dependency converged; a router that
// failed for good counts as settled, so one lost plugin cannot hold a transition open.
func (s *supervisor[P, M]) desiredStateTransitionReadyToCommit() bool {
	return s.transition == SupervisorTransitionPreparing &&
		s.pendingDesiredState.desiredRevision == 0 &&
		s.reconciler.status.snapshotGeneration == s.transitionGeneration &&
		s.reconciler.status.availability == runtime.AvailabilityReady &&
		s.desiredState.desiredRevision != 0 &&
		s.desiredState.snapshotGeneration == s.transitionGeneration &&
		s.reconciler.status.revision == s.desiredState.desiredRevision &&
		s.catalog.status.desiredRevision == s.desiredState.desiredRevision &&
		s.catalog.status.settledRouters == s.catalog.status.desiredRouters
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
		s.catalog.status.settledRouters != s.catalog.status.desiredRouters {
		return
	}
	s.transition = SupervisorTransitionIdle
}

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
		s.lifecycle != SupervisorDraining &&
		s.lifecycle != SupervisorStopped &&
		s.transition == SupervisorTransitionIdle &&
		s.projectionReady() &&
		s.catalog.pid != (gen.PID{})
}

// reconcilerActorName returns the desired-state reconciler actor name.
func (s *supervisor[P, M]) reconcilerActorName() gen.Atom {
	return ReconcilerActorName(s.namespace)
}

// snapshotSupervisorName is derived from the followed namespace, not from this runtime's name.
func (s *supervisor[P, M]) snapshotSupervisorName() gen.Atom {
	return snapshot.SupervisorName(s.namespace)
}

// catalogActorName returns the catalog actor name.
func (s *supervisor[P, M]) catalogActorName() gen.Atom {
	return CatalogActorName(s.namespace)
}

// supervisorStateReader gates on routability, not full readiness: one lost plugin must not stop
// callers from invoking healthy ones, and its own invocations still fail with ErrPluginUnavailable.
func (s *supervisor[P, M]) supervisorStateReader() bool {
	return s.projection.ReadyGeneration != 0 &&
		s.projection.ReadyGeneration == s.projection.CommittedGeneration &&
		s.projection.Status.CommittedGeneration == s.projection.ReadyGeneration &&
		s.projection.Status.Availability.Routable() &&
		s.desiredState.snapshotGeneration == s.projection.ReadyGeneration &&
		s.catalog.status.desiredRevision == s.desiredState.desiredRevision &&
		s.catalog.status.availability.Routable() &&
		s.transition == SupervisorTransitionIdle &&
		s.lifecycle != SupervisorDraining &&
		s.lifecycle != SupervisorStopped
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// startSnapshotSupervisor records a new snapshot supervisor incarnation.
func (s *supervisor[P, M]) startSnapshotSupervisor(pid gen.PID) {
	if s.snapshot.Pid == pid {
		return
	}
	s.snapshot.Pid = pid
	s.lastProjectionStatusEpoch = 0
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
}

// startReconcilerActor initializes a desired-state reconciler incarnation.
func (s *supervisor[P, M]) startReconcilerActor(pid gen.PID) error {
	state := &s.reconciler
	if state.pid == pid {
		return nil
	}
	state.statusEpoch = 0
	if s.transition != SupervisorTransitionIdle {
		s.transition = SupervisorTransitionPreparing
	}
	state.pid = pid
	state.status = reconcilerActorStatus{
		err:          state.status.err,
		lifecycle:    ReconcilerActorStarting,
		availability: runtime.AvailabilityUnavailable,
	}

	revisionBase := max(s.pendingDesiredState.desiredRevision, s.desiredState.desiredRevision)
	if err := s.SendWithPriority(pid, MessageReconcilerActorActivate{revisionBase: revisionBase}, gen.MessagePriorityHigh); err != nil {
		state.status.err = err
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
	if state.pid == pid {
		return nil
	}
	state.statusEpoch = 0
	state.pid = pid
	state.status = newCatalogStatus(state.status.err)

	if err := s.SendWithPriority(pid, MessageCatalogActivate{}, gen.MessagePriorityHigh); err != nil {
		state.status.err = err
		_ = s.Node().SendExit(pid, fmt.Errorf("activate catalog: %w", err))
		return nil
	}
	if s.desiredState.desiredRevision != 0 {
		if err := s.SendWithPriority(pid, s.desiredState, gen.MessagePriorityHigh); err != nil {
			state.status.err = err
			_ = s.Node().SendExit(pid, fmt.Errorf("replay desired state to catalog: %w", err))
			return nil
		}
	}
	if s.lifecycle == SupervisorDraining {
		if err := s.Send(pid, MessageDrain{}); err != nil {
			state.status.err = err
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
	state.status.lifecycle = CatalogActorRestarting
	state.status.availability = runtime.AvailabilityUnavailable
	state.status.err = reason

	for callID, call := range s.inFlightCalls {
		if call.catalog == pid {
			s.finishCall(callID, ErrPluginUnavailable)
		}
	}
}

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
		// Status precedes the child's commit result; only that or the deadline resolves it.
		s.projection.ReadyGeneration = 0
		s.projection.Status = status
		return s.beginPendingDesiredStateTransition()
	}
	if !pidChanged && status.Lifecycle == snapshot.ProjectionActorRunning &&
		status.Availability.Routable() &&
		status.CommittedGeneration == s.projection.CommittedGeneration &&
		s.projection.ReadyGeneration == s.projection.CommittedGeneration {
		s.projection.Status = status
		s.finishDesiredStateTransition()
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
	s.labels.Count(s, metricProjectionCommits, telemetry.Result(m.Err))
	if m.Err != nil {
		if s.adoptAuthoritativeProjectionPID(m.ProjectionPID) {
			s.err = m.Err
			s.cancelProjectionDeadline()
			s.cancelProjectionCommitRetry(false)
			return s.requestProjectionCommit()
		}
		if m.ProjectionPID != s.projection.PendingPID || m.ProjectionPID != s.projection.Pid {
			return nil
		}
		if s.projection.ReadyGeneration == m.Generation {
			return nil
		}
		s.err = m.Err
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
	s.err = nil
	s.projection.PendingGeneration = 0
	s.projection.PendingPID = gen.PID{}
	s.cancelProjectionCommitRetry(true)
	s.finishDesiredStateTransition()
	if err := s.beginPendingDesiredStateTransition(); err != nil {
		return err
	}
	return nil
}

// requestProjectionCommit asks the snapshot supervisor to commit the projection.
func (s *supervisor[P, M]) requestProjectionCommit() error {
	if s.Process == nil ||
		s.lifecycle == SupervisorDraining ||
		s.lifecycle == SupervisorStopped ||
		s.snapshot.Pid == (gen.PID{}) ||
		s.projection.CommittedGeneration == 0 ||
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
	if err := s.SendWithPriority(s.snapshot.Pid, snapshot.MessageProjectionCommit{
		Generation:    s.projection.PendingGeneration,
		ProjectionPID: s.projection.PendingPID,
	}, gen.MessagePriorityHigh); err != nil {
		s.err = err
		return s.scheduleProjectionCommitRetry()
	}
	if err := s.scheduleProjectionDeadline(); err != nil {
		s.err = err
		s.projection.PendingGeneration = 0
		s.projection.PendingPID = gen.PID{}
		return s.scheduleProjectionCommitRetry()
	}
	return nil
}

// scheduleProjectionCommitRetry schedules another projection commit attempt.
func (s *supervisor[P, M]) scheduleProjectionCommitRetry() error {
	if s.lifecycle == SupervisorDraining ||
		s.lifecycle == SupervisorStopped ||
		s.projection.Retry.Pending ||
		s.projection.CommittedGeneration == 0 {
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
	cancel, err := s.SendWithPriorityAfter(s.PID(), MessageProjectionCommitRetry{token: token}, gen.MessagePriorityHigh, delay)
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
	cancel, err := s.SendWithPriorityAfter(s.PID(), MessageProjectionCommitDeadline{
		token:         token,
		generation:    s.projection.PendingGeneration,
		projectionPID: s.projection.PendingPID,
	}, gen.MessagePriorityHigh, delay)
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
// Status
// ---------------------------------------------------------------------------

// newCatalogStatus returns an initial catalog status for an actor incarnation.
func newCatalogStatus(err error) catalogActorStatus {
	return catalogActorStatus{
		lifecycle:    CatalogActorStarting,
		availability: runtime.AvailabilityUnavailable,
		err:          err,
		routers:      make(map[string]routerActorStatus),
	}
}

// mergeCatalogStatus merges the latest catalog status into supervisor state.
func (s *supervisor[P, M]) mergeCatalogStatus(status catalogActorStatus) {
	state := &s.catalog
	next := status.clone()
	if next.lifecycle == CatalogActorRunning {
		if s.lifecycle != SupervisorDraining &&
			s.lifecycle != SupervisorStopped {
			s.lifecycle = SupervisorRunning
		}
	} else if next.err == nil {
		next.err = state.status.err
	}
	state.status = next
}

// status computes the current runtime status. Nothing caches it: the supervisor publishes no status
// message, so a query is answered from live state rather than from whatever the last callback left.
func (s *supervisor[P, M]) status() SupervisorStatus {
	lifecycle := s.lifecycle
	if lifecycle == "" {
		lifecycle = SupervisorStarting
	}
	return SupervisorStatus{
		err:             runtime.FirstError(s.err, s.catalog.status.err, s.reconciler.status.err, s.projection.Status.Err),
		Lifecycle:       lifecycle,
		Availability:    s.runtimeAvailability(),
		DesiredRevision: s.currentDesiredRevision(),
		Transition:      s.transition,
		Catalog:         s.catalog.status.clone(),
		Reconciler:      s.reconciler.status,
	}
}

// reconcileStatus records the projection, then refreshes gauges and readiness after each callback.
func (s *supervisor[P, M]) reconcileStatus() {
	s.lastStatus = s.status()
	s.publishGauges()
	s.propagateReadiness()
}

// propagateReadiness updates Radar from current subtree health without publishing status messages.
func (s *supervisor[P, M]) propagateReadiness() {
	s.signal.SetReady(s, s.lifecycle == SupervisorRunning && s.runtimeAvailability() == runtime.AvailabilityReady)
}

// publishGauges publishes current values without changing state or propagating status.
func (s *supervisor[P, M]) publishGauges() {
	processesReady, processesDesired, queueDepth, activeCalls := s.routeTotals()
	incompleteCalls, unreleasedCalls := s.callCounts()
	runtimeGauges{
		lifecycle:              s.lifecycle,
		availability:           s.runtimeAvailability(),
		transition:             s.transition,
		desiredRevision:        s.currentDesiredRevision(),
		readyGeneration:        s.projection.ReadyGeneration,
		committedGeneration:    s.projection.CommittedGeneration,
		inFlightCalls:          incompleteCalls,
		unreleasedCalls:        unreleasedCalls,
		reconcilerAvailability: s.reconciler.status.availability,
		reconcilerGeneration:   s.reconciler.status.snapshotGeneration,
		reconcilerRevision:     s.reconciler.status.revision,
		catalogAvailability:    s.catalog.status.availability,
		routersDesired:         s.catalog.status.desiredRouters,
		routersRoutable:        s.catalog.status.routableRouters,
		routersSettled:         s.catalog.status.settledRouters,
		routersUnavailable:     s.catalog.status.unavailableRouters,
		processesReady:         processesReady,
		processesDesired:       processesDesired,
		queueDepth:             queueDepth,
		activeCalls:            activeCalls,
	}.publish(s.labels, s)
}

// routeTotals sums every route under every router, since these gauges are per runtime, not per deployment.
func (s *supervisor[P, M]) routeTotals() (ready, desired, queued, active int) {
	for _, router := range s.catalog.status.routers {
		for _, route := range []deploymentRouteStatus{router.primary, router.candidate} {
			ready += route.readyProcs
			desired += route.desiredProcesses
			queued += route.queueDepth
			active += route.activeCalls
		}
	}
	return ready, desired, queued, active
}

// reconcileRadar registers whatever radar is still missing, then heartbeats the readiness signal.
func (s *supervisor[P, M]) reconcileRadar() {
	if !s.collectorsRegistered {
		// Registered through the node: radar deletes a dead registrant's metrics.
		if err := telemetry.Register(s.Node(), runtimeMetrics); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
		s.collectorsRegistered = true
		s.watchRadar(telemetry.MetricsProcess)
	}
	if !s.signal.Registered() {
		if !s.watchRadar(telemetry.HealthProcess) {
			return
		}
		if err := s.signal.Register(s); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
	}
	s.radarLogged = false
	s.propagateReadiness()
	s.signal.Heartbeat(s)
}

// watchRadar monitors one radar process and reports whether the watch is installed.
func (s *supervisor[P, M]) watchRadar(name gen.Atom) bool {
	if err := s.MonitorProcessID(gen.ProcessID{Name: name, Node: s.Node().Name()}); err != nil && !errors.Is(err, gen.ErrTargetExist) {
		s.Log().Debug("radar monitor unavailable: namespace=%q process=%s error=%v", s.namespace, name, err)
		return false
	}
	return true
}

// radarUnavailableOnce logs only the first failure of an outage.
func (s *supervisor[P, M]) radarUnavailableOnce(err error) {
	if s.radarLogged {
		return
	}
	s.radarLogged = true
	s.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", s.namespace, err)
}

// HandleInspect exposes the subtree's lifecycle, its children's status, and what it still owes callers.
func (s *supervisor[P, M]) HandleInspect(gen.PID, ...string) map[string]string {
	status := s.status()
	incompleteCalls, unreleasedCalls := s.callCounts()
	return map[string]string{
		"runtime:err":                             runtime.ErrorText(status.err),
		"runtime:lifecycle":                       string(status.Lifecycle),
		"runtime:availability":                    string(status.Availability),
		"runtime:readiness_signal":                s.signal.State(),
		"runtime:desired_revision":                fmt.Sprintf("%d", status.DesiredRevision),
		"runtime:transition":                      status.Transition.String(),
		"runtime:catalog:err":                     runtime.ErrorText(status.Catalog.err),
		"runtime:reconciler:err":                  runtime.ErrorText(status.Reconciler.err),
		"runtime:projection:err":                  runtime.ErrorText(s.projection.Status.Err),
		"runtime:catalog:lifecycle":               string(status.Catalog.lifecycle),
		"runtime:catalog:availability":            string(status.Catalog.availability),
		"runtime:catalog:routers":                 fmt.Sprintf("%d", status.Catalog.desiredRouters),
		"runtime:reconciler:lifecycle":            string(status.Reconciler.lifecycle),
		"runtime:reconciler:availability":         string(status.Reconciler.availability),
		"runtime:projection:ready_generation":     fmt.Sprintf("%d", s.projection.ReadyGeneration),
		"runtime:projection:committed_generation": fmt.Sprintf("%d", s.projection.CommittedGeneration),
		"runtime:in_flight_calls":                 fmt.Sprintf("%d", incompleteCalls),
		"runtime:unreleased_calls":                fmt.Sprintf("%d", unreleasedCalls),
		"runtime:drain_waiters":                   fmt.Sprintf("%d", len(s.drainWaiters)),
	}
}

// runtimeAvailability derives runtime availability from child component status.
func (s *supervisor[P, M]) runtimeAvailability() runtime.Availability {
	if s.lifecycle == SupervisorDraining ||
		s.lifecycle == SupervisorStopped ||
		s.transition != SupervisorTransitionIdle ||
		!s.projectionReady() ||
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

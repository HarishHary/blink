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

// SupervisorStatus is the authoritative public status the runtime supervisor publishes.
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

// runtimeDrainWaiter tracks a caller waiting for runtime drain completion.
type runtimeDrainWaiter struct {
	pid gen.PID
	ref gen.Ref
}

// alive reports whether the drain waiter can still receive a response.
func (t runtimeDrainWaiter) alive() bool { return t.ref.IsAlive() }

// runtimeCall tracks an in-flight runtime call.
type runtimeCall struct {
	catalog gen.PID
	result  *runtime.AsyncResult
	cancel  context.CancelFunc
}

// supervisor coordinates the runtime actor subtree.
type supervisor[P Artifact, M any] struct {
	act.Supervisor
	opts                      SupervisorOptions
	namespace                 string
	adapter                   *Adapter[P]
	lifecycle                 SupervisorLifecycle
	transition                SupervisorTransitionPhase
	liveStatus                SupervisorStatus
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
	labels                    telemetry.Labels
	signal                    telemetry.Signal
	collectorsRegistered      bool
	radarLogged               bool
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

// MessageSubmitInvocation requests execution of a plugin invocation.
type MessageSubmitInvocation[T Artifact] struct {
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
	name := SupervisorName(s.namespace)
	if err := s.RegisterName(name); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register runtime supervisor %q: %w", name, err)
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
		return SupervisorStatusResponse{Status: s.liveStatus.clone()}, nil

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
		if !s.acceptsSubmission(m.expectedGeneration) {
			s.labels.Count(s, metricInvocationsRejected, "closed")
			m.result.Complete(ErrPluginUnavailable)
			return nil
		}
		if err := m.context.Err(); err != nil {
			s.labels.Count(s, metricInvocationsRejected, "context")
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
			s.finishCall(m.callID, ErrPluginUnavailable)
			_ = s.Node().SendExit(
				catalogPID,
				fmt.Errorf("forward invocation to catalog: %w", err),
			)
		}

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
		}

	case s.reconcilerActorName():
		s.labels.Count(s, metricChildTerminations, "reconciler", telemetry.TerminationReason(reason))
		s.retireReconcilerActor(pid)

	case s.catalogActorName():
		s.labels.Count(s, metricChildTerminations, "catalog", telemetry.TerminationReason(reason))
		s.retireCatalogActor(pid, reason)
	}
	return nil
}

// Terminate stops the runtime supervisor and completes outstanding calls.
func (s *supervisor[P, M]) Terminate(reason error) {
	defer s.reconcileStatus()
	s.lifecycle = SupervisorDraining
	s.cancelProjectionCommitRetry(false)
	s.cancelProjectionDeadline()
	for callID := range s.inFlightCalls {
		s.finishCall(callID, ErrPluginUnavailable)
	}
}

// ---------------------------------------------------------------------------
// Work
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
	s.labels.Count(s, metricInvocations, telemetry.Result(err))
	call.result.Complete(err)
	_ = s.promotePendingDesiredState()
}

// promotePendingDesiredState applies the latest proposal after tracked calls drain.
func (s *supervisor[P, M]) promotePendingDesiredState() error {
	if s.lifecycle == SupervisorDraining || s.transition == SupervisorTransitionIdle || len(s.inFlightCalls) != 0 ||
		s.pendingDesiredState.desiredRevision == 0 ||
		s.pendingDesiredState.snapshotGeneration != s.transitionGeneration {
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
	if s.lifecycle == SupervisorDraining || s.pendingDesiredState.desiredRevision == 0 || !s.pendingProjectionReady() {
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
		s.lifecycle != SupervisorDraining && s.transition == SupervisorTransitionIdle && s.projectionReady() &&
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
		s.transition == SupervisorTransitionIdle && s.lifecycle != SupervisorDraining
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
		lifecycle:    ReconcilerActorStarting,
		availability: runtime.AvailabilityUnavailable,
	}

	revisionBase := max(s.pendingDesiredState.desiredRevision, s.desiredState.desiredRevision)
	if err := s.SendWithPriority(pid, MessageReconcilerActorActivate{revisionBase: revisionBase}, gen.MessagePriorityHigh); err != nil {
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
	state.status = newCatalogStatus(state.status.lastError)

	if err := s.SendWithPriority(pid, MessageCatalogActivate{}, gen.MessagePriorityHigh); err != nil {
		_ = s.Node().SendExit(pid, fmt.Errorf("activate catalog: %w", err))
		return nil
	}
	if s.desiredState.desiredRevision != 0 {
		if err := s.SendWithPriority(pid, s.desiredState, gen.MessagePriorityHigh); err != nil {
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
	state.status.lifecycle = CatalogActorRestarting
	state.status.availability = runtime.AvailabilityUnavailable
	state.status.lastError = reason

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
	if err := s.SendWithPriority(s.snapshot.Pid, snapshot.MessageProjectionCommit{
		Generation:    s.projection.PendingGeneration,
		ProjectionPID: s.projection.PendingPID,
	}, gen.MessagePriorityHigh); err != nil {
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

// status computes the current publishable runtime status, shared by reconcileStatus (which caches it as
// liveStatus) and HandleInspect (to an operator).
func (s *supervisor[P, M]) status() SupervisorStatus {
	lifecycle := s.lifecycle
	if lifecycle == "" {
		lifecycle = SupervisorStarting
	}
	return SupervisorStatus{
		Lifecycle:       lifecycle,
		Availability:    s.runtimeAvailability(),
		DesiredRevision: s.currentDesiredRevision(),
		Transition:      s.transition,
		Catalog:         s.catalog.status.clone(),
		Reconciler:      s.reconciler.status,
	}
}

// reconcileStatus refreshes the queryable status, gauges, and readiness after each callback.
func (s *supervisor[P, M]) reconcileStatus() {
	s.liveStatus = s.status()
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
	runtimeGauges{
		lifecycle:              s.lifecycle,
		availability:           s.runtimeAvailability(),
		transition:             s.transition,
		desiredRevision:        s.currentDesiredRevision(),
		readyGeneration:        s.projection.ReadyGeneration,
		committedGeneration:    s.projection.CommittedGeneration,
		inFlightCalls:          len(s.inFlightCalls),
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

// routeTotals sums every route under every router, since these gauges are per runtime, not per
// deployment.
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

// HandleInspect exposes lifecycle, both children's sub-status, the projection generations, and the
// in-flight call and drain-waiter counts.
func (s *supervisor[P, M]) HandleInspect(gen.PID, ...string) map[string]string {
	status := s.status()
	return map[string]string{
		"runtime:lifecycle":                       string(status.Lifecycle),
		"runtime:availability":                    string(status.Availability),
		"runtime:readiness_signal":                s.signal.State(),
		"runtime:desired_revision":                fmt.Sprintf("%d", status.DesiredRevision),
		"runtime:transition":                      fmt.Sprintf("%d", status.Transition),
		"runtime:catalog:lifecycle":               string(status.Catalog.lifecycle),
		"runtime:catalog:availability":            string(status.Catalog.availability),
		"runtime:catalog:routers":                 fmt.Sprintf("%d", status.Catalog.desiredRouters),
		"runtime:reconciler:lifecycle":            string(status.Reconciler.lifecycle),
		"runtime:reconciler:availability":         string(status.Reconciler.availability),
		"runtime:projection:ready_generation":     fmt.Sprintf("%d", s.projection.ReadyGeneration),
		"runtime:projection:committed_generation": fmt.Sprintf("%d", s.projection.CommittedGeneration),
		"runtime:in_flight_calls":                 fmt.Sprintf("%d", len(s.inFlightCalls)),
		"runtime:drain_waiters":                   fmt.Sprintf("%d", len(s.drainWaiters)),
	}
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

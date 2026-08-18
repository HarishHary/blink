package plugin

import (
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// CatalogActorLifecycle describes one router-catalog actor incarnation.
type CatalogActorLifecycle string

const (
	CatalogActorStarting   CatalogActorLifecycle = "starting"
	CatalogActorRunning    CatalogActorLifecycle = "running"
	CatalogActorRestarting CatalogActorLifecycle = "restarting"
	CatalogActorDraining   CatalogActorLifecycle = "draining"
	CatalogActorStopped    CatalogActorLifecycle = "stopped"
)

// catalogActorState tracks the catalog actor incarnation and status.
type catalogActorState struct {
	pid             gen.PID
	actorGeneration uint64
	lastEpoch       uint64
	status          CatalogActorStatus
}

// CatalogActorStatus is owned by catalogActor, except for LastError, which is
// owned by runtimeSupervisor because the supervisor replaces catalog actor
// incarnations.
type CatalogActorStatus struct {
	Lifecycle          CatalogActorLifecycle
	Availability       runtime.Availability
	Generation         uint64
	LastError          error
	Revision           uint64
	DesiredRouters     int
	RoutableRouters    int
	DegradedRouters    int
	UnavailableRouters int
	Routers            map[string]RouterActorStatus
}

// clone deep-copies router statuses so a receiver cannot mutate catalog state.
func (s CatalogActorStatus) clone() CatalogActorStatus {
	clone := s
	clone.Routers = make(map[string]RouterActorStatus, len(s.Routers))
	for id, status := range s.Routers {
		clone.Routers[id] = status.clone()
	}
	return clone
}

// ---------------------------------------------------------------------------
// Catalog actor
// ---------------------------------------------------------------------------

// catalogActor owns router actors and projects their aggregate status.
type catalogActor[T Syncable] struct {
	act.Actor
	opts            CatalogOptions[T]
	generation      uint64
	desiredRevision uint64
	activated       bool
	draining        bool
	drainReported   bool
	liveStatus      CatalogActorStatus
	statusEpoch     uint64
	routers         map[string]*routerState
	desired         map[string]MessageApplyRouterDesiredState
	inFlightCalls   map[uint64]gen.PID
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageCatalogActivate promotes the catalog actor to a new generation.
type MessageCatalogActivate struct{ generation uint64 }

// MessageApplyCatalogDesiredState delivers the desired router state to apply.
type MessageApplyCatalogDesiredState struct {
	desiredRevision    uint64
	snapshotGeneration int64
	desired            map[string]MessageApplyRouterDesiredState
}

// MessageCatalogDrained reports that the catalog actor has fully drained.
type MessageCatalogDrained struct {
	pid        gen.PID
	generation uint64
}

// MessageCatalogStatusChanged publishes an epoch-ordered catalog status update.
type MessageCatalogStatusChanged struct {
	pid        gen.PID
	generation uint64
	epoch      uint64
	status     CatalogActorStatus
}

// MessageRouterRestart re-drives a pending router restart after backoff.
type MessageRouterRestart struct {
	pluginID        string
	desiredRevision uint64
	token           uint64
}

// newCatalogActor creates a catalog actor with its runtime options.
func newCatalogActor[T Syncable](opts CatalogOptions[T]) gen.ProcessBehavior {
	return &catalogActor[T]{opts: opts}
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init allocates the catalog actor's router, desired-state, and call indexes.
func (a *catalogActor[T]) Init(...any) error {
	a.opts = catalogOptionsWithDefaults(a.opts)
	a.routers = make(map[string]*routerState)
	a.desired = make(map[string]MessageApplyRouterDesiredState)
	a.inFlightCalls = make(map[uint64]gen.PID)
	return nil
}

// ---------------------------------------------------------------------------
// Message ingress
// ---------------------------------------------------------------------------

// HandleMessage receives catalog administration, router facts, and lifecycle messages.
func (a *catalogActor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageCatalogActivate:
		if m.generation <= a.generation {
			return nil
		}
		if a.activated {
			return fmt.Errorf(
				"catalog already activated as generation %d",
				a.generation,
			)
		}
		a.generation = m.generation
		a.activated = true
		a.reconcileStatus()

	case MessageApplyCatalogDesiredState:
		if !a.activated || a.draining || m.desiredRevision < a.desiredRevision {
			return nil
		}
		a.desiredRevision = m.desiredRevision
		a.desired = m.desired
		for id := range m.desired {
			if err := a.sendDesiredToRouter(id); err != nil {
				return err
			}
		}
		for id, ref := range a.routers {
			if _, ok := m.desired[id]; ok {
				continue
			}
			ref.retiring = true
			a.cancelRouterRestartBackoff(id, false)
			if ref.pid == (gen.PID{}) {
				a.retireRouter(id, runtime.ErrPluginUnavailable)
				continue
			}
			_ = a.SendWithPriority(ref.pid, MessageDrain{}, gen.MessagePriorityHigh)
		}
		a.reconcileStatus()

	case MessageInvokePlugin[T]:
		if a.draining || a.desiredRevision == 0 {
			a.finishUntrackedCall(m, runtime.ErrPluginUnavailable)
			return nil
		}
		ref := a.routers[m.PluginID]
		if ref == nil || ref.pid == (gen.PID{}) || ref.retiring {
			a.finishUntrackedCall(m, runtime.ErrPluginUnavailable)
			return nil
		}
		a.inFlightCalls[m.CallID] = ref.pid
		if err := a.Send(ref.pid, m); err != nil {
			a.finishTrackedCall(m.CallID, runtime.ErrPluginUnavailable)
			_ = a.Node().SendExit(ref.pid, fmt.Errorf("forward invocation to router: %w", err))
		}

	case MessageCancelInvocation:
		routerPID, ok := a.inFlightCalls[m.CallID]
		if !ok {
			return nil
		}
		if err := a.SendWithPriority(routerPID, m, gen.MessagePriorityHigh); err != nil {
			a.finishTrackedCall(m.CallID, m.Err)
		}

	case MessageInvocationCompleted:
		if routerPID, ok := a.inFlightCalls[m.CallID]; ok && from == routerPID {
			delete(a.inFlightCalls, m.CallID)
			_ = a.Send(a.Parent(), m)
		}

	case MessageDrain:
		if a.draining {
			return nil
		}
		a.draining = true
		a.cancelAllRouterRestarts(false)
		a.reconcileStatus()
		if a.liveRouterCount() == 0 {
			a.reportDrained()
			return nil
		}
		for _, ref := range a.routers {
			if ref.pid == (gen.PID{}) {
				continue
			}
			ref.retiring = true
			_ = a.SendWithPriority(ref.pid, MessageDrain{}, gen.MessagePriorityHigh)
		}

	case MessageRouterDrained:
		ref := a.routers[m.pluginID]
		if ref == nil ||
			ref.pid != m.pid ||
			ref.generation != m.generation {
			return nil
		}
		if !a.draining && !ref.retiring {
			return nil
		}

		_ = a.SendWithPriority(ref.pid, MessageStop{}, gen.MessagePriorityHigh)
		a.retireRouter(m.pluginID, runtime.ErrPluginUnavailable)

		if a.draining {
			a.reconcileStatus()
			if a.liveRouterCount() == 0 {
				a.reportDrained()
			}
			return nil
		}

		if _, desired := a.desired[m.pluginID]; desired {
			if err := a.sendDesiredToRouter(m.pluginID); err != nil {
				return err
			}
		}
		a.reconcileStatus()

	case MessageRouterStatusChanged:
		ref := a.routers[m.pluginID]
		if ref == nil ||
			ref.pid != m.pid ||
			ref.generation != m.generation ||
			m.epoch <= ref.lastEpoch {
			return nil
		}
		ref.lastEpoch = m.epoch

		next := m.status.clone()
		a.cancelRouterRestartBackoff(m.pluginID, true)
		next.LastError = nil
		ref.status = next
		a.reconcileStatus()

	case MessageRouterRestart:
		ref := a.routers[m.pluginID]
		if ref == nil ||
			ref.restart == nil ||
			!ref.restart.Pending ||
			ref.restart.Token != m.token {
			return nil
		}
		ref.restart.Pending = false
		ref.restart.Cancel = nil
		if !a.draining && m.desiredRevision == a.desiredRevision {
			return a.sendDesiredToRouter(m.pluginID)
		}

	case MessageStop:
		return gen.TerminateReasonNormal

	case gen.MessageDownPID:
		for id, ref := range a.routers {
			if ref.pid != m.PID {
				continue
			}

			ref.status.Lifecycle = RouterActorRestarting
			ref.status.Availability = runtime.AvailabilityUnavailable
			ref.status.LastError = m.Reason

			_, desired := a.desired[id]
			if a.draining || ref.retiring || !desired {
				a.retireRouter(id, runtime.ErrPluginUnavailable)
				if a.draining {
					if a.liveRouterCount() == 0 {
						a.reportDrained()
					}
				} else if desired {
					if err := a.sendDesiredToRouter(id); err != nil {
						return err
					}
				}
			} else {
				ref.pid = gen.PID{}
				ref.lastEpoch = 0
				for callID, routerPID := range a.inFlightCalls {
					if routerPID == m.PID {
						a.finishTrackedCall(callID, runtime.ErrPluginUnavailable)
					}
				}
				if err := a.scheduleRouterRestart(id); err != nil {
					return err
				}
			}
			a.reconcileStatus()
			break
		}
	}
	return nil
}

// HandleCall rejects synchronous calls because the catalog exposes no call API.
func (a *catalogActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported catalog call %T", request), nil
}

// ---------------------------------------------------------------------------
// Desired-state reconciliation
// ---------------------------------------------------------------------------

// sendDesiredToRouter ensures a router is running and sends it the desired state.
func (a *catalogActor[T]) sendDesiredToRouter(id string) error {
	desired, ok := a.desired[id]
	if !ok || a.draining {
		return nil
	}
	if ref := a.routers[id]; ref != nil && ref.pid != (gen.PID{}) && ref.retiring {
		return nil
	}
	ref, err := a.startRouter(id)
	if err != nil {
		return a.scheduleRouterRestart(id)
	}
	ref.retiring = false
	desired.desiredRevision = a.desiredRevision
	if err := a.SendWithPriority(ref.pid, desired, gen.MessagePriorityHigh); err != nil {
		_ = a.Node().SendExit(ref.pid, fmt.Errorf("apply desired state to router %q: %w", id, err))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Router lifecycle
// ---------------------------------------------------------------------------

// startRouter creates, monitors, and activates the router for one plugin.
func (a *catalogActor[T]) startRouter(id string) (*routerState, error) {
	ref := a.routers[id]
	if ref != nil && ref.pid != (gen.PID{}) {
		return ref, nil
	}
	if ref == nil {
		ref = &routerState{}
		a.routers[id] = ref
	}

	ref.generation++
	generation := ref.generation
	ref.lastEpoch = 0
	ref.retiring = false
	prevErr := ref.status.LastError
	ref.status = RouterActorStatus{
		Lifecycle:    RouterActorStarting,
		Availability: runtime.AvailabilityUnavailable,
		LastError:    prevErr,
		Primary: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[gen.PID]DeploymentWorkerStatus),
		},
		Candidate: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[gen.PID]DeploymentWorkerStatus),
		},
	}
	a.reconcileStatus()

	pid, err := a.Spawn(func() gen.ProcessBehavior {
		return &routerActor[T]{
			opts:     a.opts.RouterOptions,
			pluginID: id,
		}
	}, gen.ProcessOptions{LinkParent: true})
	if err != nil {
		ref.status.Lifecycle = RouterActorRestarting
		ref.status.LastError = fmt.Errorf("spawn router: %w", err)
		return ref, err
	}

	ref.pid = pid
	if err := a.MonitorPID(pid); err != nil {
		_ = a.Node().SendExit(pid, gen.TerminateReasonShutdown)
		ref.pid = gen.PID{}
		ref.status.Lifecycle = RouterActorRestarting
		ref.status.LastError = fmt.Errorf("monitor router: %w", err)
		return ref, err
	}
	if err := a.SendWithPriority(pid, MessageRouterActivate{generation: generation}, gen.MessagePriorityHigh); err != nil {
		_ = a.Node().SendExit(pid, fmt.Errorf("activate router: %w", err))
		return ref, err
	}
	return ref, nil
}

// retireRouter clears a router and fails its in-flight calls.
func (a *catalogActor[T]) retireRouter(id string, callErr error) {
	ref := a.routers[id]
	if ref == nil {
		return
	}

	if ref.restart != nil {
		ref.restart.CancelScheduled(false)
	}

	retiredPID := ref.pid
	if retiredPID != (gen.PID{}) {
		for callID, routerPID := range a.inFlightCalls {
			if routerPID == retiredPID {
				a.finishTrackedCall(callID, callErr)
			}
		}
	}

	ref.pid = gen.PID{}
	ref.lastEpoch = 0
	ref.retiring = false
	prevErr := ref.status.LastError
	ref.status = RouterActorStatus{
		Lifecycle:    RouterActorStopped,
		Availability: runtime.AvailabilityUnavailable,
		LastError:    prevErr,
		Revision:     a.desiredRevision,
		Primary: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[gen.PID]DeploymentWorkerStatus),
		},
		Candidate: DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[gen.PID]DeploymentWorkerStatus),
		},
	}
}

// ---------------------------------------------------------------------------
// Router restart scheduling
// ---------------------------------------------------------------------------

// routerRestartState returns the restart backoff state for a router.
func (a *catalogActor[T]) routerRestartState(id string) *runtime.ScheduledBackoff {
	ref := a.routers[id]
	if ref == nil {
		ref = &routerState{}
		a.routers[id] = ref
	}
	if ref.restart == nil {
		ref.restart = runtime.NewScheduledBackoff(a.opts.RetryMin, a.opts.RetryMax)
	}
	return ref.restart
}

// scheduleRouterRestart schedules a retry for an unavailable router.
func (a *catalogActor[T]) scheduleRouterRestart(id string) error {
	if a.draining {
		return nil
	}
	state := a.routerRestartState(id)
	if state.Pending {
		return nil
	}

	delay := state.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("router restart for %q: %w", id, runtime.ErrBackoffStopped)
	}
	state.Token++
	token := state.Token
	cancel, err := a.SendAfter(
		a.PID(),
		MessageRouterRestart{pluginID: id, desiredRevision: a.desiredRevision, token: token},
		delay,
	)
	if err != nil {
		return fmt.Errorf("schedule router restart for %q: %w", id, err)
	}
	state.Pending = true
	state.Cancel = cancel
	if ref := a.routers[id]; ref != nil {
		ref.status.Lifecycle = RouterActorRestarting
		ref.status.Availability = runtime.AvailabilityUnavailable
	}
	a.reconcileStatus()
	return nil
}

// cancelRouterRestartBackoff cancels a router restart and optionally resets its backoff.
func (a *catalogActor[T]) cancelRouterRestartBackoff(id string, reset bool) {
	if ref := a.routers[id]; ref != nil {
		if ref.restart != nil {
			ref.restart.CancelScheduled(reset)
		}
	}
}

// cancelAllRouterRestarts cancels every router restart and optionally resets backoff.
func (a *catalogActor[T]) cancelAllRouterRestarts(reset bool) {
	for id := range a.routers {
		a.cancelRouterRestartBackoff(id, reset)
	}
}

// ---------------------------------------------------------------------------
// Invocation routing
// ---------------------------------------------------------------------------

// liveRouterCount returns the number of routers with live actor PIDs.
func (a *catalogActor[T]) liveRouterCount() int {
	count := 0
	for _, ref := range a.routers {
		if ref.pid != (gen.PID{}) {
			count++
		}
	}
	return count
}

// finishUntrackedCall reports an unavailable invocation that was never routed.
func (a *catalogActor[T]) finishUntrackedCall(call MessageInvokePlugin[T], err error) {
	_ = a.Send(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: err})
}

// finishTrackedCall removes and reports a completed invocation.
func (a *catalogActor[T]) finishTrackedCall(callID uint64, err error) {
	if _, ok := a.inFlightCalls[callID]; !ok {
		return
	}
	delete(a.inFlightCalls, callID)
	_ = a.Send(a.Parent(), MessageInvocationCompleted{CallID: callID, Err: err})
}

// ---------------------------------------------------------------------------
// Status projection
// ---------------------------------------------------------------------------

// reconcileStatus recomputes and publishes the aggregate catalog status.
func (a *catalogActor[T]) reconcileStatus() {
	routers := make(map[string]RouterActorStatus, len(a.desired))
	routable := 0
	degraded := 0
	unavailable := 0

	for id := range a.desired {
		ref := a.routers[id]
		if ref == nil {
			unavailable++
			routers[id] = RouterActorStatus{
				Lifecycle:    RouterActorStarting,
				Availability: runtime.AvailabilityUnavailable,
			}
			continue
		}

		status := ref.status.clone()
		routers[id] = status
		if status.NormalRoutable {
			routable++
		}
		switch status.Availability {
		case runtime.AvailabilityDegraded:
			degraded++
		case runtime.AvailabilityUnavailable:
			unavailable++
		}
	}

	lifecycle := CatalogActorStarting
	switch {
	case a.drainReported:
		lifecycle = CatalogActorStopped
	case a.draining:
		lifecycle = CatalogActorDraining
	case a.desiredRevision != 0:
		lifecycle = CatalogActorRunning
	}

	availability := runtime.AvailabilityUnavailable
	switch {
	case a.desiredRevision == 0:
		availability = runtime.AvailabilityUnavailable
	case len(a.desired) == 0:
		availability = runtime.AvailabilityReady
	case unavailable == 0 && degraded == 0 && routable == len(a.desired):
		availability = runtime.AvailabilityReady
	case routable > 0:
		availability = runtime.AvailabilityDegraded
	}

	next := CatalogActorStatus{
		Lifecycle:          lifecycle,
		Availability:       availability,
		Generation:         a.generation,
		Revision:           a.desiredRevision,
		DesiredRouters:     len(a.desired),
		RoutableRouters:    routable,
		DegradedRouters:    degraded,
		UnavailableRouters: unavailable,
		Routers:            routers,
	}
	if sameCatalogStatus(a.liveStatus, next) && a.statusEpoch != 0 {
		return
	}

	a.statusEpoch++
	a.liveStatus = next
	if !a.activated {
		return
	}
	_ = a.Send(a.Parent(), MessageCatalogStatusChanged{
		pid:        a.PID(),
		generation: a.generation,
		epoch:      a.statusEpoch,
		status:     next.clone(),
	})
}

// sameCatalogStatus reports whether two catalog statuses are equal.
func sameCatalogStatus(left, right CatalogActorStatus) bool {
	if left.Lifecycle != right.Lifecycle ||
		left.Availability != right.Availability ||
		left.Generation != right.Generation ||
		left.Revision != right.Revision ||
		left.DesiredRouters != right.DesiredRouters ||
		left.RoutableRouters != right.RoutableRouters ||
		left.DegradedRouters != right.DegradedRouters ||
		left.UnavailableRouters != right.UnavailableRouters ||
		len(left.Routers) != len(right.Routers) {
		return false
	}
	for id, leftRouter := range left.Routers {
		rightRouter, ok := right.Routers[id]
		if !ok || !sameRouterStatus(leftRouter, rightRouter) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Draining
// ---------------------------------------------------------------------------

// reportDrained announces catalog drain completion to the parent actor.
func (a *catalogActor[T]) reportDrained() {
	if a.drainReported {
		return
	}
	a.drainReported = true
	a.reconcileStatus()
	_ = a.Send(a.Parent(), MessageCatalogDrained{pid: a.PID(), generation: a.generation})
}

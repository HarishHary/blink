package plugin

import (
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
	pid       gen.PID
	lastEpoch uint64
	status    catalogActorStatus
}

// catalogActorStatus is owned by catalogActor, except for LastError, which is
// owned by runtimeSupervisor because the supervisor replaces catalog actor
// incarnations.
type catalogActorStatus struct {
	lifecycle          CatalogActorLifecycle
	availability       runtime.Availability
	lastError          error
	desiredRevision    uint64
	desiredRouters     int
	routableRouters    int
	degradedRouters    int
	unavailableRouters int
	settledRouters     int
	routers            map[string]routerActorStatus
}

// clone deep-copies router statuses so a receiver cannot mutate catalog state.
func (s catalogActorStatus) clone() catalogActorStatus {
	clone := s
	clone.routers = make(map[string]routerActorStatus, len(s.routers))
	for id, status := range s.routers {
		clone.routers[id] = status.clone()
	}
	return clone
}

// ---------------------------------------------------------------------------
// Catalog actor
// ---------------------------------------------------------------------------

// catalogActor owns router actors and projects their aggregate status.
type catalogActor[T Artifact] struct {
	act.Actor
	opts            CatalogOptions
	adapter         *Adapter[T]
	desiredRevision uint64
	activated       bool
	draining        bool
	drainReported   bool
	lastStatus      catalogActorStatus
	statusEpoch     uint64
	routers         map[string]*routerState
	desired         map[string]routerDesiredState
	inFlightCalls   map[uint64]gen.PID
	labels          telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageCatalogActivate lets the catalog actor publish status.
type MessageCatalogActivate struct{}

// MessageApplyCatalogDesiredState delivers the desired router state to apply.
type MessageApplyCatalogDesiredState struct {
	desiredRevision    uint64
	snapshotGeneration int64
	desired            map[string]routerDesiredState
}

// MessageCatalogDrained reports that the catalog actor has fully drained.
type MessageCatalogDrained struct {
	pid gen.PID
}

// MessageCatalogStatusChanged publishes an epoch-ordered catalog status update.
type MessageCatalogStatusChanged struct {
	pid    gen.PID
	epoch  uint64
	status catalogActorStatus
}

// MessageRouterRestart re-drives a pending router restart after backoff.
type MessageRouterRestart struct {
	pluginID        string
	desiredRevision uint64
	token           uint64
}

// newCatalogActor creates a catalog actor with its runtime options.
func newCatalogActor[T Artifact](opts CatalogOptions, adapter *Adapter[T], labels telemetry.Labels) gen.ProcessBehavior {
	return &catalogActor[T]{opts: opts, adapter: adapter, labels: labels}
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init allocates the catalog actor's router, desired-state, and call indexes.
func (a *catalogActor[T]) Init(...any) error {
	a.opts = catalogOptionsWithDefaults(a.opts)
	a.routers = make(map[string]*routerState)
	a.desired = make(map[string]routerDesiredState)
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
		if a.activated {
			return nil
		}
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
		next.lastError = nil
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

			a.labels.Count(a, metricRouterTerminations, telemetry.TerminationReason(m.Reason))
			ref.status.lifecycle = RouterActorRestarting
			ref.status.availability = runtime.AvailabilityUnavailable
			ref.status.lastError = m.Reason

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
	return fmt.Errorf("unsupported catalog call %T", request), nil
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
	message := MessageApplyRouterDesiredState{
		desiredRevision:    a.desiredRevision,
		routerDesiredState: desired,
	}
	if err := a.SendWithPriority(ref.pid, message, gen.MessagePriorityHigh); err != nil {
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
	prevErr := ref.status.lastError
	ref.status = routerActorStatus{
		lifecycle:    RouterActorStarting,
		availability: runtime.AvailabilityUnavailable,
		lastError:    prevErr,
		primary: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessStatus),
		},
		candidate: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessStatus),
		},
	}
	a.reconcileStatus()

	pid, err := a.Spawn(func() gen.ProcessBehavior {
		return &routerActor[T]{
			opts:     a.opts.RouterOptions,
			adapter:  a.adapter,
			pluginID: id,
			labels:   a.labels,
		}
	}, gen.ProcessOptions{LinkParent: true})
	if err != nil {
		ref.status.lifecycle = RouterActorRestarting
		ref.status.lastError = fmt.Errorf("spawn router: %w", err)
		return ref, err
	}

	ref.pid = pid
	if err := a.MonitorPID(pid); err != nil {
		_ = a.Node().SendExit(pid, gen.TerminateReasonShutdown)
		ref.pid = gen.PID{}
		ref.status.lifecycle = RouterActorRestarting
		ref.status.lastError = fmt.Errorf("monitor router: %w", err)
		return ref, err
	}
	if err := a.SendWithPriority(pid, MessageRouterActivate{generation: generation}, gen.MessagePriorityHigh); err != nil {
		_ = a.Node().SendExit(pid, fmt.Errorf("activate router: %w", err))
		return ref, err
	}
	a.labels.Count(a, metricRouterStarts)
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
	prevErr := ref.status.lastError
	ref.status = routerActorStatus{
		lifecycle:    RouterActorStopped,
		availability: runtime.AvailabilityUnavailable,
		lastError:    prevErr,
		revision:     a.desiredRevision,
		primary: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessStatus),
		},
		candidate: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessStatus),
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
	a.labels.Count(a, metricRouterRestarts)
	if ref := a.routers[id]; ref != nil {
		ref.status.lifecycle = RouterActorRestarting
		ref.status.availability = runtime.AvailabilityUnavailable
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

// status computes the current aggregate catalog status from desired and router state, shared by reconcileStatus (to the supervisor) and HandleInspect (to an operator).
func (a *catalogActor[T]) status() catalogActorStatus {
	routers := make(map[string]routerActorStatus, len(a.desired))
	routable := 0
	degraded := 0
	unavailable := 0
	settled := 0

	for id := range a.desired {
		ref := a.routers[id]
		if ref == nil {
			unavailable++
			routers[id] = routerActorStatus{
				lifecycle:    RouterActorStarting,
				availability: runtime.AvailabilityUnavailable,
			}
			continue
		}

		status := ref.status.clone()
		routers[id] = status
		if status.normalRoutable {
			routable++
		}
		if routerSettled(status, a.desiredRevision) {
			settled++
		}
		switch status.availability {
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

	return catalogActorStatus{
		lifecycle:          lifecycle,
		availability:       availability,
		desiredRevision:    a.desiredRevision,
		desiredRouters:     len(a.desired),
		routableRouters:    routable,
		degradedRouters:    degraded,
		unavailableRouters: unavailable,
		settledRouters:     settled,
		routers:            routers,
	}
}

// reconcileStatus recomputes and publishes the aggregate catalog status.
func (a *catalogActor[T]) reconcileStatus() {
	next := a.status()
	if sameCatalogStatus(a.lastStatus, next) && a.statusEpoch != 0 {
		return
	}

	a.statusEpoch++
	a.lastStatus = next
	if !a.activated {
		return
	}
	_ = a.Send(a.Parent(), MessageCatalogStatusChanged{
		pid:    a.PID(),
		epoch:  a.statusEpoch,
		status: next.clone(),
	})
}

// HandleInspect exposes concise catalog operational state: aggregate router health plus the desired-vs-actual router count and in-flight call depth a Ready status alone doesn't distinguish.
func (a *catalogActor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	return map[string]string{
		"catalog:lifecycle":        string(status.lifecycle),
		"catalog:availability":     string(status.availability),
		"catalog:desired_revision": fmt.Sprintf("%d", status.desiredRevision),
		"catalog:routers":          fmt.Sprintf("%d/%d", len(a.routers), status.desiredRouters),
		"catalog:routable":         fmt.Sprintf("%d", status.routableRouters),
		"catalog:degraded":         fmt.Sprintf("%d", status.degradedRouters),
		"catalog:unavailable":      fmt.Sprintf("%d", status.unavailableRouters),
		"catalog:settled":          fmt.Sprintf("%d", status.settledRouters),
		"catalog:in_flight_calls":  fmt.Sprintf("%d", len(a.inFlightCalls)),
	}
}

// routerSettled reports whether a router is done moving toward revision: it reached that
// revision and either routes or has failed for good. Failed counts as done because a route
// that has spent its restart budget never recovers on its own, so a caller waiting for the
// whole catalog to be healthy would wait forever on it. Starting and restarting still may
// go either way, so they are not settled.
func routerSettled(status routerActorStatus, revision uint64) bool {
	if status.revision != revision {
		return false
	}
	return status.availability == runtime.AvailabilityReady ||
		status.primary.lifecycle == DeploymentRouteFailed ||
		status.candidate.lifecycle == DeploymentRouteFailed
}

// sameCatalogStatus reports whether two catalog statuses are equal.
func sameCatalogStatus(left, right catalogActorStatus) bool {
	if left.lifecycle != right.lifecycle ||
		left.availability != right.availability ||
		left.desiredRevision != right.desiredRevision ||
		left.desiredRouters != right.desiredRouters ||
		left.routableRouters != right.routableRouters ||
		left.degradedRouters != right.degradedRouters ||
		left.unavailableRouters != right.unavailableRouters ||
		left.settledRouters != right.settledRouters ||
		len(left.routers) != len(right.routers) {
		return false
	}
	for id, leftRouter := range left.routers {
		rightRouter, ok := right.routers[id]
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
	_ = a.Send(a.Parent(), MessageCatalogDrained{pid: a.PID()})
}

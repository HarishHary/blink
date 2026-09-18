package plugin

import (
	"fmt"
	"maps"
	"slices"

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
	pid         gen.PID
	status      catalogActorStatus
	statusEpoch int64
}

// catalogActorStatus reports aggregate router health; the supervisor also records incarnation failures.
type catalogActorStatus struct {
	lifecycle          CatalogActorLifecycle
	availability       runtime.Availability
	desiredRevision    uint64
	desiredRouters     int
	routableRouters    int
	degradedRouters    int
	unavailableRouters int
	settledRouters     int
	routers            map[string]routerActorStatus
	err                error
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

// catalogInvocation is one call forwarded to a router, kept until that router reports the call's
// execution capacity released rather than only its result.
type catalogInvocation struct {
	router    gen.PID
	completed bool
}

// catalogActor owns router actors and projects their aggregate status.
type catalogActor[T Artifact] struct {
	act.Actor
	opts            CatalogOptions
	adapter         *Adapter[T]
	desiredRevision uint64
	activated       bool
	routers         map[string]*routerState
	desired         map[string]routerDesiredState
	inFlightCalls   map[uint64]catalogInvocation
	lifecycle       CatalogActorLifecycle // the catalog's own live lifecycle; the supervisor owns restarting
	err             error                 // the catalog's own failure, kept apart from its routers' errors
	lastStatus      catalogActorStatus    // last published projection, the baseline reconcileStatus dedupes against
	lastStatusEpoch int64
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

// MessageCatalogStatusChanged publishes a changed catalog status to the supervisor.
type MessageCatalogStatusChanged struct {
	pid         gen.PID
	status      catalogActorStatus
	statusEpoch int64
}

// MessageRouterRestart re-drives a pending router restart after backoff.
type MessageRouterRestart struct {
	pluginID        string
	desiredRevision uint64
	token           uint64
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// newCatalogActor creates a catalog actor with its runtime options.
func newCatalogActor[T Artifact](opts CatalogOptions, adapter *Adapter[T], labels telemetry.Labels) gen.ProcessBehavior {
	return &catalogActor[T]{opts: opts, adapter: adapter, labels: labels}
}

// Init allocates the catalog actor's router, desired-state, and call indexes.
func (a *catalogActor[T]) Init(...any) error {
	a.opts = catalogOptionsWithDefaults(a.opts)
	a.routers = make(map[string]*routerState)
	a.desired = make(map[string]routerDesiredState)
	a.inFlightCalls = make(map[uint64]catalogInvocation)
	a.lifecycle = CatalogActorStarting
	return nil
}

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
		if !a.activated || a.isDraining() || m.desiredRevision < a.desiredRevision {
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
				a.retireRouter(id, ErrPluginUnavailable)
				continue
			}
			_ = a.SendWithPriority(ref.pid, MessageDrain{}, gen.MessagePriorityHigh)
		}
		a.reconcileStatus()

	case MessageInvokePlugin[T]:
		if a.isDraining() || a.desiredRevision == 0 {
			a.finishUntrackedCall(m, ErrPluginUnavailable)
			return nil
		}
		ref := a.routers[m.PluginID]
		if ref == nil || ref.pid == (gen.PID{}) || ref.retiring {
			a.finishUntrackedCall(m, ErrPluginUnavailable)
			return nil
		}
		a.inFlightCalls[m.CallID] = catalogInvocation{router: ref.pid}
		if err := a.Send(ref.pid, m); err != nil {
			a.finishTrackedCall(m.CallID, ErrPluginUnavailable)
			_ = a.Node().SendExit(ref.pid, fmt.Errorf("forward invocation to router: %w", err))
		}

	case MessageCancelInvocation:
		call, ok := a.inFlightCalls[m.CallID]
		if !ok {
			return nil
		}
		if err := a.SendWithPriority(call.router, m, gen.MessagePriorityHigh); err != nil {
			a.finishTrackedCall(m.CallID, m.Err)
		}

	case MessageInvocationCompleted:
		call, ok := a.inFlightCalls[m.CallID]
		if !ok || from != call.router || call.completed {
			return nil
		}
		// An executing call stays tracked: its router still owes the release that frees plugin capacity.
		if m.Executing {
			call.completed = true
			a.inFlightCalls[m.CallID] = call
		} else {
			delete(a.inFlightCalls, m.CallID)
		}
		_ = a.SendWithPriority(a.Parent(), m, gen.MessagePriorityHigh)

	case MessageInvocationReleased:
		call, ok := a.inFlightCalls[m.CallID]
		if !ok || from != call.router || !call.completed {
			return nil
		}
		delete(a.inFlightCalls, m.CallID)
		_ = a.SendWithPriority(a.Parent(), m, gen.MessagePriorityHigh)

	case MessageDrain:
		if a.isDraining() {
			return nil
		}
		a.lifecycle = CatalogActorDraining
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
		if !a.isDraining() && !ref.retiring {
			return nil
		}

		_ = a.SendWithPriority(ref.pid, MessageStop{}, gen.MessagePriorityHigh)
		a.retireRouter(m.pluginID, ErrPluginUnavailable)

		if a.isDraining() {
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
			from != ref.pid ||
			ref.pid != m.pid ||
			ref.generation != m.generation || m.statusEpoch <= ref.statusEpoch {
			return nil
		}
		ref.statusEpoch = m.statusEpoch

		next := m.status.clone()
		a.cancelRouterRestartBackoff(m.pluginID, true)
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
		if !a.isDraining() && m.desiredRevision == a.desiredRevision {
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
			ref.status.err = m.Reason

			_, desired := a.desired[id]
			if a.isDraining() || ref.retiring || !desired {
				a.retireRouter(id, ErrPluginUnavailable)
				if a.isDraining() {
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
				for callID, call := range a.inFlightCalls {
					if call.router == m.PID {
						a.finishTrackedCall(callID, ErrPluginUnavailable)
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

// Terminate marks the catalog stopped and cancels pending router restarts; the supervisor fails the
// in-flight calls this incarnation owned, so they are not touched here.
func (a *catalogActor[T]) Terminate(reason error) {
	defer a.reconcileStatus()
	a.err = runtime.FirstError(reason, a.err)
	a.lifecycle = CatalogActorStopped
	a.cancelAllRouterRestarts(false)
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// sendDesiredToRouter ensures a router is running and sends it the desired state.
func (a *catalogActor[T]) sendDesiredToRouter(id string) error {
	desired, ok := a.desired[id]
	if !ok || a.isDraining() {
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
	_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: err}, gen.MessagePriorityHigh)
}

// finishTrackedCall removes an invocation and reports its result, or only its release when the result
// was already reported while the plugin was still executing.
func (a *catalogActor[T]) finishTrackedCall(callID uint64, err error) {
	call, ok := a.inFlightCalls[callID]
	if !ok {
		return
	}
	delete(a.inFlightCalls, callID)
	if call.completed {
		_ = a.SendWithPriority(a.Parent(), MessageInvocationReleased{CallID: callID}, gen.MessagePriorityHigh)
		return
	}
	_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: callID, Err: err}, gen.MessagePriorityHigh)
}

// reportDrained announces catalog drain completion to the parent actor.
func (a *catalogActor[T]) reportDrained() {
	if a.lifecycle == CatalogActorStopped {
		return
	}
	a.lifecycle = CatalogActorStopped
	a.reconcileStatus()
	_ = a.SendWithPriority(a.Parent(), MessageCatalogDrained{pid: a.PID()}, gen.MessagePriorityHigh)
}

// ---------------------------------------------------------------------------
// Recovery
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
	ref.statusEpoch = 0
	generation := ref.generation
	ref.retiring = false
	prevErr := ref.status.err
	ref.status = routerActorStatus{
		lifecycle:    RouterActorStarting,
		availability: runtime.AvailabilityUnavailable,
		err:          prevErr,
		primary: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessActorStatus),
		},
		candidate: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessActorStatus),
		},
	}
	a.reconcileStatus()

	// Each start attempt supersedes the catalog's own previous failure.
	a.err = nil
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
		a.err = fmt.Errorf("spawn router %s: %w", id, err)
		return ref, err
	}

	ref.pid = pid
	if err := a.MonitorPID(pid); err != nil {
		_ = a.Node().SendExit(pid, gen.TerminateReasonShutdown)
		ref.pid = gen.PID{}
		ref.status.lifecycle = RouterActorRestarting
		a.err = fmt.Errorf("monitor router %s: %w", id, err)
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
		for callID, call := range a.inFlightCalls {
			if call.router == retiredPID {
				a.finishTrackedCall(callID, callErr)
			}
		}
	}

	ref.pid = gen.PID{}
	ref.retiring = false
	prevErr := ref.status.err
	ref.status = routerActorStatus{
		lifecycle:    RouterActorStopped,
		availability: runtime.AvailabilityUnavailable,
		err:          prevErr,
		revision:     a.desiredRevision,
		primary: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessActorStatus),
		},
		candidate: deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessActorStatus),
		},
	}
}

// routerRestartState returns the restart backoff state for a router.
func (a *catalogActor[T]) routerRestartState(id string) *runtime.ScheduledBackoff {
	ref := a.routers[id]
	if ref == nil {
		ref = &routerState{}
		a.routers[id] = ref
	}
	if ref.restart == nil {
		ref.restart = runtime.NewScheduledBackoff(a.opts.RestartMin, a.opts.RestartMax)
	}
	return ref.restart
}

// scheduleRouterRestart schedules a retry for an unavailable router.
func (a *catalogActor[T]) scheduleRouterRestart(id string) error {
	if a.isDraining() {
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
	cancel, err := a.SendWithPriorityAfter(
		a.PID(),
		MessageRouterRestart{pluginID: id, desiredRevision: a.desiredRevision, token: token},
		gen.MessagePriorityHigh,
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
// Status
// ---------------------------------------------------------------------------

// status computes the aggregate catalog status.
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

	// The catalog's own failure wins, then its routers in plugin-id order.
	errors := make([]error, 0, len(routers)+1)
	errors = append(errors, a.err)
	for _, id := range slices.Sorted(maps.Keys(routers)) {
		errors = append(errors, routers[id].err)
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
	// A stopped catalog serves nothing, whatever its routers' cached statuses still claim.
	if a.lifecycle == CatalogActorStopped {
		availability = runtime.AvailabilityUnavailable
	}

	return catalogActorStatus{
		lifecycle:          a.lifecycle,
		availability:       availability,
		desiredRevision:    a.desiredRevision,
		desiredRouters:     len(a.desired),
		routableRouters:    routable,
		degradedRouters:    degraded,
		unavailableRouters: unavailable,
		settledRouters:     settled,
		routers:            routers,
		err:                runtime.FirstError(errors...),
	}
}

// reconcileStatus recomputes and publishes the aggregate catalog status.
func (a *catalogActor[T]) reconcileStatus() {
	if a.desiredRevision != 0 && a.lifecycle == CatalogActorStarting {
		a.lifecycle = CatalogActorRunning
	}
	next := a.status()
	if sameCatalogActorStatus(a.lastStatus, next) {
		return
	}
	a.lastStatusEpoch = runtime.NextStatusEpoch(a.lastStatusEpoch)
	a.lastStatus = next
	a.propagateStatus(next)
}

// isDraining reports whether the catalog has closed admission for shutdown.
func (a *catalogActor[T]) isDraining() bool {
	return a.lifecycle == CatalogActorDraining || a.lifecycle == CatalogActorStopped
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (a *catalogActor[T]) propagateStatus(next catalogActorStatus) {
	if !a.activated {
		return
	}
	_ = a.SendWithPriority(a.Parent(), MessageCatalogStatusChanged{
		statusEpoch: a.lastStatusEpoch,
		pid:         a.PID(),
		status:      next.clone(),
	}, gen.MessagePriorityHigh)
}

// HandleInspect exposes the desired-vs-actual router counts a Ready status alone does not distinguish.
func (a *catalogActor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	return map[string]string{
		"catalog:err":              runtime.ErrorText(status.err),
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

// routerSettled reports whether a router reached revision and either routes or failed for good; a
// route that spent its restart budget never recovers, so waiting on it would wait forever.
func routerSettled(status routerActorStatus, revision uint64) bool {
	if status.revision != revision {
		return false
	}
	return status.availability == runtime.AvailabilityReady ||
		status.primary.lifecycle == DeploymentRouteFailed ||
		status.candidate.lifecycle == DeploymentRouteFailed
}

// sameCatalogActorStatus compares publishable status, including aggregate failures.
func sameCatalogActorStatus(left, right catalogActorStatus) bool {
	if left.lifecycle != right.lifecycle ||
		left.availability != right.availability ||
		runtime.ErrorText(left.err) != runtime.ErrorText(right.err) ||
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
		if !ok || !sameRouterActorStatus(leftRouter, rightRouter) {
			return false
		}
	}
	return true
}

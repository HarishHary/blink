package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// routerState tracks one router actor and its restart state.
type routerState struct {
	pid         gen.PID
	generation  uint64
	restart     *runtime.ScheduledBackoff
	status      routerActorStatus
	statusEpoch int64
	retiring    bool
}

// RouterActorLifecycle describes one logical-plugin router actor incarnation.
type RouterActorLifecycle string

const (
	RouterActorStarting   RouterActorLifecycle = "starting"
	RouterActorRunning    RouterActorLifecycle = "running"
	RouterActorRestarting RouterActorLifecycle = "restarting"
	RouterActorDraining   RouterActorLifecycle = "draining"
	RouterActorStopped    RouterActorLifecycle = "stopped"
)

// routerActorStatus is the catalog-facing router status contract.
type routerActorStatus struct {
	lifecycle      RouterActorLifecycle
	availability   runtime.Availability
	revision       uint64
	normalRoutable bool
	shadowRoutable bool
	primary        deploymentRouteStatus
	candidate      deploymentRouteStatus
	err            error
}

// clone deep-copies the nested route statuses so a receiver cannot mutate router state.
func (s routerActorStatus) clone() routerActorStatus {
	clone := s
	clone.primary = s.primary.clone()
	clone.candidate = s.candidate.clone()
	return clone
}

// deploymentRoutePhase is the lifecycle phase of a single dynamic route.
type deploymentRoutePhase uint8

const (
	deploymentRoutePending deploymentRoutePhase = iota
	deploymentRouteActive
	deploymentRouteDraining
	deploymentRouteRemoving
)

// deploymentRouteState is one dynamic route: a stable address that outlives many manager PIDs.
type deploymentRouteState struct {
	key         DeploymentRouteKey
	deployment  Deployment
	name        gen.Atom
	pid         gen.PID
	restart     *runtime.ScheduledBackoff
	status      deploymentManagerActorStatus
	statusEpoch int64
	phase       deploymentRoutePhase
	managers    map[gen.PID]struct{}
}

// routerActor owns one dynamic Ergo route per concrete DeploymentRouteKey.
type routerActor[T Artifact] struct {
	act.Router
	opts             RouterOptions
	pluginID         string
	generation       uint64
	desiredRevision  uint64
	adapter          *Adapter[T]
	routesByKey      map[DeploymentRouteKey]*deploymentRouteState
	routesByName     map[gen.Atom]DeploymentRouteKey
	inFlightCalls    map[uint64]*routerInvocation
	desiredPrimary   *Deployment
	desiredCandidate *Deployment
	activePrimary    *Deployment
	activeCandidate  *Deployment
	lifecycle        RouterActorLifecycle // the router's own live lifecycle
	err              error                // the router's own failure, kept apart from its routes' errors
	lastStatus       routerActorStatus    // last published projection, the baseline reconcileStatus dedupes against
	lastStatusEpoch  int64
	labels           telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// routerInvocation tracks one in-flight call routed to a manager, awaiting accept/complete.
type routerInvocation struct {
	route    gen.Atom
	ackToken uint64
	ackStop  gen.CancelFunc
	accepted bool
	manager  gen.PID
}

// MessageInvocationTimedOut fires when a routed call is not accepted before its deadline.
type MessageInvocationTimedOut struct {
	callID uint64
	token  uint64
}

// MessageRouterActivate promotes the router to a new generation and lets it publish status.
type MessageRouterActivate struct{ generation uint64 }

// MessageRetryDeployment asks the router to nudge a specific manager to retry.
type MessageRetryDeployment struct {
	key     DeploymentRouteKey
	manager gen.PID
}

// routerDesiredState describes the desired primary and candidate deployments.
type routerDesiredState struct {
	primary           *Deployment
	candidate         *Deployment
	primaryDeferred   bool
	candidateDeferred bool
}

// MessageApplyRouterDesiredState delivers one catalog revision to a router.
type MessageApplyRouterDesiredState struct {
	desiredRevision uint64
	routerDesiredState
}

// MessageRouterDrained reports to the catalog that the router has fully drained.
type MessageRouterDrained struct {
	pluginID   string
	pid        gen.PID
	generation uint64
}

// MessageRouterStatusChanged publishes a changed router status to the catalog.
type MessageRouterStatusChanged struct {
	pluginID    string
	pid         gen.PID
	generation  uint64
	status      routerActorStatus
	statusEpoch int64
}

// MessageRetryRouteStep is the self-timer that re-drives a route's pending lifecycle step.
type MessageRetryRouteStep struct {
	route gen.Atom
	token uint64
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// Init allocates the router's route and in-flight-call indexes.
func (a *routerActor[T]) Init(...any) (act.RouterOptions, error) {
	a.opts = routerOptionsWithDefaults(a.opts)
	a.lifecycle = RouterActorStarting
	a.routesByKey = make(map[DeploymentRouteKey]*deploymentRouteState)
	a.routesByName = make(map[gen.Atom]DeploymentRouteKey)
	a.inFlightCalls = make(map[uint64]*routerInvocation)
	return act.RouterOptions{}, nil
}

// Terminate cancels pending timers for in-flight calls and route restarts.
func (a *routerActor[T]) Terminate(reason error) {
	defer a.reconcileStatus()
	a.err = runtime.FirstError(reason, a.err)
	a.lifecycle = RouterActorStopped
	for _, call := range a.inFlightCalls {
		if call.ackStop != nil {
			call.ackStop()
		}
	}
	for _, ref := range a.routesByKey {
		if ref.restart != nil {
			ref.restart.CancelScheduled(false)
		}
	}
}

// RouteMessage routes normal-priority plugin invocations.
func (a *routerActor[T]) RouteMessage(_ gen.PID, message any) gen.Atom {
	switch m := message.(type) {
	case MessageInvokePlugin[T]:
		return a.routeInvocation(m)
	default:
		return act.RouteDiscard
	}
}

// RouteCall discards synchronous calls; the router serves none over the route path.
func (a *routerActor[T]) RouteCall(_ gen.PID, _ gen.Ref, _ any) gen.Atom { return act.RouteDiscard }

// HandleMessage receives every administration and route-lifecycle fact at High/Max priority.
func (a *routerActor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageInvocationTimedOut:
		call := a.inFlightCalls[m.callID]
		if call != nil && !call.accepted && call.ackToken == m.token {
			a.labels.Count(a, metricAcceptanceTimeouts)
			a.finishTrackedCall(m.callID, ErrPluginUnavailable)
		}

	case MessageRetryRouteStep:
		a.retryRouteStep(m)

	case MessageRouterActivate:
		if m.generation <= a.generation {
			return nil
		}
		if a.generation != 0 {
			return fmt.Errorf("router %q already activated as generation %d", a.pluginID, a.generation)
		}
		a.generation = m.generation
		a.reconcileStatus()

	case MessageApplyRouterDesiredState:
		if a.generation == 0 || a.isDraining() || m.desiredRevision < a.desiredRevision {
			return nil
		}
		for _, d := range []*Deployment{m.primary, m.candidate} {
			if d != nil && d.Id != a.pluginID {
				return fmt.Errorf("deployment %q belongs to plugin %q, router owns %q", d.Name, d.Id, a.pluginID)
			}
		}
		a.desiredRevision = m.desiredRevision
		if !m.primaryDeferred {
			a.desiredPrimary = m.primary
			if err := a.applyDeployment(m.primary); err != nil {
				return err
			}
		}
		if !m.candidateDeferred {
			a.desiredCandidate = m.candidate
			if err := a.applyDeployment(m.candidate); err != nil {
				return err
			}
		}
		a.reconcileDeployments()
		a.reconcileStatus()

	case MessageCancelInvocation:
		a.cancelInvocation(m)

	case MessageRetryDeployment:
		ref := a.routesByKey[m.key]
		if ref == nil {
			return nil
		}
		if pid := a.refreshRoutePID(ref); pid != (gen.PID{}) && m.manager == pid {
			_ = a.SendWithPriority(pid, MessageDeploymentManagerRetry{route: ref.name, manager: pid}, gen.MessagePriorityHigh)
		}

	case MessageDeploymentManagerStatusChanged:
		if ref, ok := a.currentManager(m.route, from, m.manager); ok {
			if m.statusEpoch <= ref.statusEpoch {
				return nil
			}
			ref.statusEpoch = m.statusEpoch
			ref.status = m.status
			ref.managers[m.manager] = struct{}{}
			if ref.restart != nil {
				ref.restart.CancelScheduled(true)
			}
			if ref.phase == deploymentRouteDraining {
				a.drainRoute(ref)
			}
			a.reconcileDeployments()
			a.reconcileStatus()
		} else if key, ok := a.routesByName[m.route]; ok {
			ref := a.routesByKey[key]
			if ref != nil && from == m.manager && ref.phase == deploymentRouteActive {
				if _, known := ref.managers[m.manager]; known && a.refreshRoutePID(ref) == (gen.PID{}) {
					_ = a.scheduleRouteStep(ref)
				}
			}
		}

	case MessageInvocationAccepted:
		if ref, ok := a.currentManager(m.route, from, m.manager); ok {
			ref.managers[m.manager] = struct{}{}
			if ref.phase == deploymentRouteDraining {
				a.drainRoute(ref)
			}
			a.acceptInvocation(m)
		}

	case MessageInvocationCompleted:
		a.completeInvocation(from, m)

	case MessageDeploymentManagerDrained:
		if ref, ok := a.currentManager(m.route, from, m.manager); ok && ref.phase == deploymentRouteDraining {
			a.removeDrainedRoute(ref)
			a.reconcileDeployments()
			a.reconcileStatus()
			a.reportDrained()
		}

	case MessageDeploymentManagerTerminated:
		a.deploymentManagerTerminated(from, m)

	case act.MessageRouteFailed:
		if from != a.PID() {
			return nil
		}
		call, ok := m.Message.(MessageInvokePlugin[T])
		if !ok {
			return nil
		}
		tracked := a.inFlightCalls[call.CallID]
		if tracked == nil || tracked.route != m.Name {
			return nil
		}
		a.finishTrackedCall(call.CallID, ErrPluginUnavailable)
		if key, ok := a.routesByName[m.Name]; ok {
			ref := a.routesByKey[key]
			if ref == nil || ref.phase != deploymentRouteActive || a.refreshRoutePID(ref) != (gen.PID{}) {
				return nil
			}
			_ = a.scheduleRouteStep(ref)
		}
		a.reconcileStatus()

	case MessageDrain:
		a.beginDrain()

	case MessageStop:
		return gen.TerminateReasonNormal
	}
	return nil
}

// HandleCall rejects synchronous calls; the router exposes no request/response API.
func (a *routerActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("unsupported router call %T", request), nil
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// applyDeployment creates or updates the route backing one desired deployment.
func (a *routerActor[T]) applyDeployment(d *Deployment) error {
	if d == nil || a.isDraining() {
		return nil
	}
	key := d.RouteKey()
	name, err := deploymentRouteName(key)
	if err != nil {
		return err
	}
	if existing, ok := a.routesByName[name]; ok && existing != key {
		return fmt.Errorf("deployment route identity collision for %q", name)
	}
	ref := a.routesByKey[key]
	if ref == nil {
		ref = &deploymentRouteState{key: key, name: name, managers: make(map[gen.PID]struct{})}
		a.routesByKey[key], a.routesByName[name] = ref, key
	}
	ref.deployment = *d
	if ref.phase != deploymentRoutePending {
		return nil
	}
	return a.addRoute(ref)
}

// reconcileDeployments promotes healthy desired routes to active and drains obsolete ones.
func (a *routerActor[T]) reconcileDeployments() {
	a.activateDesired(&a.activePrimary, a.desiredPrimary)
	a.activateDesired(&a.activeCandidate, a.desiredCandidate)
	keep := make(map[DeploymentRouteKey]bool)
	for _, deployment := range []*Deployment{a.desiredPrimary, a.desiredCandidate, a.activePrimary, a.activeCandidate} {
		if deployment != nil {
			keep[deployment.RouteKey()] = true
		}
	}
	for key, ref := range a.routesByKey {
		if keep[key] || ref.phase >= deploymentRouteDraining {
			continue
		}
		a.drainRoute(ref)
	}
}

// activateDesired marks a desired deployment active once its route is running and ready.
func (a *routerActor[T]) activateDesired(active **Deployment, desired *Deployment) {
	if desired == nil {
		*active = nil
		return
	}
	ref := a.routesByKey[desired.RouteKey()]
	if ref != nil && ref.phase == deploymentRouteActive &&
		ref.status.lifecycle == DeploymentManagerActorRunning && ref.status.availability == runtime.AvailabilityReady {
		*active = desired
	}
}

// routeInvocation selects the rollout target route and tracks the call awaiting acceptance.
func (a *routerActor[T]) routeInvocation(call MessageInvokePlugin[T]) gen.Atom {
	if a.isDraining() {
		a.labels.Count(a, metricUnroutable)
		_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return act.RouteDiscard
	}
	// Shadow goes to the shadow candidate; else primary, unless a canary wins this call's bucket.
	var ref *deploymentRouteState
	target := "shadow"
	if call.Shadow {
		if a.activeCandidate != nil && a.activeCandidate.Mode == runtime.RolloutModeShadow {
			ref = a.routesByKey[a.activeCandidate.RouteKey()]
		}
	} else {
		deployment := a.activePrimary
		target = "primary"
		if a.activeCandidate != nil && a.activeCandidate.Mode == runtime.RolloutModeCanary &&
			float64(runtime.RolloutBucket(call.RolloutKey)) <= a.activeCandidate.RolloutPct {
			deployment, target = a.activeCandidate, "candidate"
		}
		if deployment != nil {
			ref = a.routesByKey[deployment.RouteKey()]
		}
	}
	if ref == nil || ref.phase != deploymentRouteActive || a.refreshRoutePID(ref) == (gen.PID{}) {
		a.labels.Count(a, metricUnroutable)
		_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return act.RouteDiscard
	}
	if _, exists := a.inFlightCalls[call.CallID]; exists {
		return act.RouteDiscard
	}
	tracked := &routerInvocation{route: ref.name, ackToken: 1}
	timeout := a.opts.DeploymentManagerOptions.DispatchTimeout
	if timeout <= 0 {
		timeout = DefaultDeploymentManagerDispatchTimeout
	}
	cancel, err := a.SendWithPriorityAfter(a.PID(), MessageInvocationTimedOut{callID: call.CallID, token: tracked.ackToken}, gen.MessagePriorityHigh, timeout)
	if err != nil {
		a.labels.Count(a, metricUnroutable)
		_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return act.RouteDiscard
	}
	tracked.ackStop = cancel
	a.inFlightCalls[call.CallID] = tracked
	a.labels.Count(a, metricRouted, target)
	return ref.name
}

// acceptInvocation binds an in-flight call to the manager that accepted it.
func (a *routerActor[T]) acceptInvocation(message MessageInvocationAccepted) {
	call := a.inFlightCalls[message.callID]
	if call == nil || call.accepted || call.route != message.route {
		return
	}
	if call.ackStop != nil {
		call.ackStop()
		call.ackStop = nil
	}
	call.accepted, call.manager = true, message.manager
}

// completeInvocation finishes an in-flight call reported done by its bound manager.
func (a *routerActor[T]) completeInvocation(from gen.PID, message MessageInvocationCompleted) {
	call := a.inFlightCalls[message.CallID]
	if call == nil || !call.accepted || call.route != message.Route || call.manager != from ||
		call.manager != message.Manager {
		return
	}
	a.finishTrackedCall(message.CallID, message.Err)
}

// cancelInvocation forwards a cancellation to the call's manager, or fails it locally.
func (a *routerActor[T]) cancelInvocation(message MessageCancelInvocation) {
	call := a.inFlightCalls[message.CallID]
	if call == nil {
		return
	}
	target := call.manager
	if !call.accepted {
		if key, ok := a.routesByName[call.route]; ok {
			target = a.refreshRoutePID(a.routesByKey[key])
		}
	}
	if target == (gen.PID{}) || a.SendWithPriority(target, message, gen.MessagePriorityHigh) != nil {
		err := message.Err
		if err == nil {
			err = context.Canceled
		}
		a.finishTrackedCall(message.CallID, err)
	}
}

// finishTrackedCall stops the acceptance timer and reports the call complete to the catalog.
func (a *routerActor[T]) finishTrackedCall(callID uint64, err error) {
	call := a.inFlightCalls[callID]
	if call == nil {
		return
	}
	if call.ackStop != nil {
		call.ackStop()
	}
	delete(a.inFlightCalls, callID)
	_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: callID, Err: err}, gen.MessagePriorityHigh)
}

// drainRoute advances one route toward teardown: quiesce its manager, then let it be removed.
func (a *routerActor[T]) drainRoute(ref *deploymentRouteState) {
	// The normal-priority drain must follow invocations already forwarded to this manager.
	switch ref.phase {
	case deploymentRoutePending:
		if ref.restart != nil {
			ref.restart.CancelScheduled(false)
		}
		delete(a.routesByKey, ref.key)
		delete(a.routesByName, ref.name)
		if a.activePrimary != nil && a.activePrimary.RouteKey() == ref.key {
			a.activePrimary = nil
		}
		if a.activeCandidate != nil && a.activeCandidate.RouteKey() == ref.key {
			a.activeCandidate = nil
		}
		return
	case deploymentRouteActive:
		ref.phase = deploymentRouteDraining
	case deploymentRouteRemoving:
		return
	}
	if ref.restart != nil {
		ref.restart.CancelScheduled(false)
	}
	if pid := a.refreshRoutePID(ref); pid != (gen.PID{}) {
		_ = a.Send(pid, MessageDrain{})
		return
	}
	// No live manager: respawn one, already draining, so it can finish the drain protocol.
	if err := a.RespawnRoute(ref.name); err != nil {
		a.err = err
		_ = a.scheduleRouteStep(ref)
		return
	}
	// Respawn may not have registered the PID yet; retry the drain once it appears.
	if pid := a.refreshRoutePID(ref); pid != (gen.PID{}) {
		_ = a.Send(pid, MessageDrain{})
		return
	}
	_ = a.scheduleRouteStep(ref)
}

// beginDrain starts a global drain of every route and reports if it is already complete.
func (a *routerActor[T]) beginDrain() {
	if a.isDraining() {
		return
	}
	a.lifecycle = RouterActorDraining
	for _, ref := range a.routesByKey {
		a.drainRoute(ref)
	}
	a.reconcileStatus()
	a.reportDrained()
}

// reportDrained announces router drain completion to the catalog once no routes remain.
func (a *routerActor[T]) reportDrained() {
	if a.lifecycle != RouterActorDraining || len(a.routesByKey) > 0 {
		return
	}
	a.lifecycle = RouterActorStopped
	a.reconcileStatus()
	_ = a.SendWithPriority(a.Parent(), MessageRouterDrained{pluginID: a.pluginID, pid: a.PID(), generation: a.generation}, gen.MessagePriorityHigh)
}

// removeDrainedRoute unregisters a drained route and clears any active pointer to it.
func (a *routerActor[T]) removeDrainedRoute(ref *deploymentRouteState) {
	ref.phase = deploymentRouteRemoving
	if err := a.RemoveRoute(ref.name); err != nil {
		a.err = err
		_ = a.scheduleRouteStep(ref)
		return
	}
	delete(a.routesByKey, ref.key)
	delete(a.routesByName, ref.name)
	if a.activePrimary != nil && a.activePrimary.RouteKey() == ref.key {
		a.activePrimary = nil
	}
	if a.activeCandidate != nil && a.activeCandidate.RouteKey() == ref.key {
		a.activeCandidate = nil
	}
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// addRoute registers the dynamic route and schedules a retry step if registration fails.
func (a *routerActor[T]) addRoute(ref *deploymentRouteState) error {
	err := a.AddRoute(act.Route{Name: ref.name, Factory: func() gen.ProcessBehavior {
		return a.newDeploymentManager(ref)
	}})
	if err != nil {
		a.err = err
		return a.scheduleRouteStep(ref)
	}
	ref.phase = deploymentRouteActive
	a.refreshRoutePID(ref)
	return nil
}

// retryRouteStep re-drives a route's pending lifecycle step after backoff, per its current phase.
func (a *routerActor[T]) retryRouteStep(message MessageRetryRouteStep) {
	key, ok := a.routesByName[message.route]
	if !ok {
		return
	}
	ref := a.routesByKey[key]
	if ref == nil || ref.restart == nil || !ref.restart.Pending || ref.restart.Token != message.token {
		return
	}
	ref.restart.Pending, ref.restart.Cancel = false, nil
	if ref.phase == deploymentRouteRemoving {
		a.removeDrainedRoute(ref)
	} else if a.isDraining() || ref.phase == deploymentRouteDraining {
		a.drainRoute(ref)
	} else {
		var err error
		if ref.phase == deploymentRoutePending {
			err = a.addRoute(ref)
		} else if ref.phase == deploymentRouteActive && a.refreshRoutePID(ref) == (gen.PID{}) {
			err = a.RespawnRoute(ref.name)
			if err == nil {
				a.refreshRoutePID(ref)
			}
		}
		if err != nil {
			a.err = err
			_ = a.scheduleRouteStep(ref)
		}
	}
	a.reconcileStatus()
}

// scheduleRouteStep arms a per-route backoff timer that re-drives its pending lifecycle step.
func (a *routerActor[T]) scheduleRouteStep(ref *deploymentRouteState) error {
	if a.isDraining() && ref.phase < deploymentRouteDraining {
		return nil
	}
	if ref.restart == nil {
		ref.restart = runtime.NewScheduledBackoff(a.opts.RetryMin, a.opts.RetryMax)
	}
	if ref.restart.Pending {
		return nil
	}
	delay := ref.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		a.err = fmt.Errorf("deployment route step for %v: %w", ref.key, runtime.ErrBackoffStopped)
		return a.err
	}
	ref.restart.Token++
	cancel, err := a.SendWithPriorityAfter(a.PID(), MessageRetryRouteStep{route: ref.name, token: ref.restart.Token}, gen.MessagePriorityHigh, delay)
	if err != nil {
		a.err = fmt.Errorf("schedule deployment route step: %w", err)
		return a.err
	}
	ref.restart.Pending, ref.restart.Cancel = true, cancel
	return nil
}

// newDeploymentManager builds the DeploymentManager child that serves this route.
func (a *routerActor[T]) newDeploymentManager(ref *deploymentRouteState) *deploymentManagerActor[T] {
	ref.status = deploymentManagerActorStatus{
		err:          ref.status.err,
		lifecycle:    DeploymentManagerActorStarting,
		availability: runtime.AvailabilityUnavailable,
		processes:    make(map[gen.PID]pluginProcessActorStatus),
	}
	return &deploymentManagerActor[T]{
		adapter:    a.adapter,
		options:    a.opts.DeploymentManagerOptions,
		deployment: ref.deployment,
		route:      ref.name,
		draining:   ref.phase >= deploymentRouteDraining || a.isDraining(),
		labels:     a.labels,
	}
}

// routePID reads the route's live manager PID without touching fencing state, so status
// projections and inspections stay free of side effects.
func (a *routerActor[T]) routePID(ref *deploymentRouteState) gen.PID {
	info, ok := a.Route(ref.name)
	if !ok {
		return gen.PID{}
	}
	return info.PID
}

// refreshRoutePID reads the route's live manager PID and records it in the fencing set.
func (a *routerActor[T]) refreshRoutePID(ref *deploymentRouteState) gen.PID {
	info, ok := a.Route(ref.name)
	if !ok {
		return gen.PID{}
	}
	if ref.pid != info.PID {
		ref.statusEpoch = 0
		ref.pid = info.PID
	}
	if info.PID != (gen.PID{}) {
		if ref.managers == nil {
			ref.managers = make(map[gen.PID]struct{})
		}
		ref.managers[info.PID] = struct{}{}
	}
	return info.PID
}

// currentManager resolves the route for an authenticated fact from its current live manager.
func (a *routerActor[T]) currentManager(route gen.Atom, from, manager gen.PID) (*deploymentRouteState, bool) {
	key, ok := a.routesByName[route]
	if !ok {
		return nil, false
	}
	ref := a.routesByKey[key]
	if ref == nil || from != manager {
		return nil, false
	}
	if ref.phase == deploymentRouteRemoving {
		return nil, false
	}
	if a.refreshRoutePID(ref) != manager {
		return nil, false
	}
	return ref, true
}

// deploymentManagerTerminated fences a manager-death fact, fails its calls, and recovers the route.
func (a *routerActor[T]) deploymentManagerTerminated(from gen.PID, message MessageDeploymentManagerTerminated) {
	key, ok := a.routesByName[message.route]
	if !ok || from != message.manager {
		return
	}
	ref := a.routesByKey[key]
	if ref == nil {
		return
	}
	if _, known := ref.managers[message.manager]; !known {
		return
	}
	delete(ref.managers, message.manager)
	if ref.pid == message.manager {
		ref.status.err = message.reason
	}
	for callID, call := range a.inFlightCalls {
		if call.accepted && call.route == message.route && call.manager == message.manager {
			a.finishTrackedCall(callID, ErrPluginUnavailable)
		}
	}
	if ref.phase == deploymentRouteDraining {
		a.drainRoute(ref)
	} else {
		if ref.phase != deploymentRouteActive || a.refreshRoutePID(ref) != (gen.PID{}) {
			return
		}
		_ = a.scheduleRouteStep(ref)
	}
	a.reconcileStatus()
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// deploymentStatusFor projects a deployment's route into the catalog-facing route status.
func (a *routerActor[T]) deploymentStatusFor(deployment *Deployment) deploymentRouteStatus {
	if deployment == nil {
		return deploymentRouteStatus{
			lifecycle:    DeploymentRouteStopped,
			availability: runtime.AvailabilityUnavailable,
			processes:    make(map[gen.PID]pluginProcessActorStatus),
		}
	}
	ref := a.routesByKey[deployment.RouteKey()]
	if ref == nil {
		return deploymentRouteStatus{
			lifecycle:        DeploymentRouteStarting,
			availability:     runtime.AvailabilityUnavailable,
			desiredProcesses: deployment.ProcessCountLimit(),
			processes:        make(map[gen.PID]pluginProcessActorStatus),
		}
	}
	lifecycle := DeploymentRouteStarting
	switch ref.status.lifecycle {
	case DeploymentManagerActorRunning:
		lifecycle = DeploymentRouteRunning
	case DeploymentManagerActorDraining:
		lifecycle = DeploymentRouteDraining
	case DeploymentManagerActorStopped:
		lifecycle = DeploymentRouteStopped
	case DeploymentManagerActorFailed:
		lifecycle = DeploymentRouteFailed
	}
	if ref.restart != nil && ref.restart.Pending {
		lifecycle = DeploymentRouteRestarting
	}
	return deploymentRouteStatus{
		lifecycle:        lifecycle,
		availability:     ref.status.availability,
		err:              ref.status.err,
		readyProcs:       ref.status.readyProcs,
		desiredProcesses: ref.status.currentProcs,
		queueDepth:       ref.status.queueDepth,
		activeCalls:      ref.status.active + ref.status.dispatching,
		processes:        ref.status.processes,
	}
}

// activeRouteAvailable reports whether a deployment's route is active with a live manager.
func (a *routerActor[T]) activeRouteAvailable(deployment *Deployment) bool {
	if deployment == nil {
		return false
	}
	ref := a.routesByKey[deployment.RouteKey()]
	return ref != nil && ref.phase == deploymentRouteActive && a.routePID(ref) != (gen.PID{})
}

// routeAvailability computes route status and routability from desired/active deployments
func (a *routerActor[T]) routeAvailability() (deploymentRouteStatus, deploymentRouteStatus, bool, bool, runtime.Availability) {
	primaryStatus, candidateStatus := a.deploymentStatusFor(a.desiredPrimary), a.deploymentStatusFor(a.desiredCandidate)
	primaryRoutable, candidateRoutable := a.activeRouteAvailable(a.activePrimary), a.activeRouteAvailable(a.activeCandidate)
	shadowRoutable := candidateRoutable && a.activeCandidate != nil && a.activeCandidate.Mode == runtime.RolloutModeShadow
	normalRoutable, fullNormalCoverage := primaryRoutable, primaryRoutable
	if candidateRoutable && a.activeCandidate != nil && a.activeCandidate.Mode == runtime.RolloutModeCanary {
		normalRoutable = normalRoutable || a.activeCandidate.RolloutPct > 0
		if a.activePrimary == nil && a.activeCandidate.RolloutPct >= 100 {
			fullNormalCoverage = true
		}
	}
	primaryHealthy := a.desiredPrimary == nil || (primaryStatus.lifecycle == DeploymentRouteRunning && primaryStatus.availability == runtime.AvailabilityReady)
	candidateHealthy := a.desiredCandidate == nil || (candidateStatus.lifecycle == DeploymentRouteRunning && candidateStatus.availability == runtime.AvailabilityReady)
	availability := runtime.AvailabilityUnavailable
	switch {
	case fullNormalCoverage && primaryHealthy && candidateHealthy:
		availability = runtime.AvailabilityReady
	case normalRoutable || shadowRoutable:
		availability = runtime.AvailabilityDegraded
	}
	return primaryStatus, candidateStatus, normalRoutable, shadowRoutable, availability
}

// status computes the router's current publishable status, shared by reconcileStatus (to the catalog)
// and HandleInspect (to an operator).
func (a *routerActor[T]) status() routerActorStatus {
	primaryStatus, candidateStatus, normalRoutable, shadowRoutable, availability := a.routeAvailability()
	if a.lifecycle == RouterActorStopped {
		availability = runtime.AvailabilityUnavailable
		normalRoutable, shadowRoutable = false, false
	}
	return routerActorStatus{
		err:            runtime.FirstError(a.err, primaryStatus.err, candidateStatus.err),
		lifecycle:      a.lifecycle,
		availability:   availability,
		revision:       a.desiredRevision,
		normalRoutable: normalRoutable,
		shadowRoutable: shadowRoutable,
		primary:        primaryStatus,
		candidate:      candidateStatus,
	}
}

// reconcileStatus recomputes router status and publishes it on change.
func (a *routerActor[T]) reconcileStatus() {
	if _, _, normalRoutable, _, _ := a.routeAvailability(); normalRoutable && a.lifecycle == RouterActorStarting {
		a.lifecycle = RouterActorRunning
	}
	next := a.status()
	if sameRouterActorStatus(a.lastStatus, next) {
		return
	}
	a.lastStatusEpoch = runtime.NextStatusEpoch(a.lastStatusEpoch)
	a.lastStatus = next
	a.propagateStatus(next)
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (a *routerActor[T]) propagateStatus(next routerActorStatus) {
	if a.generation != 0 {
		_ = a.SendWithPriority(a.Parent(), MessageRouterStatusChanged{statusEpoch: a.lastStatusEpoch, pluginID: a.pluginID, pid: a.PID(), generation: a.generation, status: next.clone()}, gen.MessagePriorityHigh)
	}
}

// HandleInspect exposes lifecycle and availability plus each route's own health, which a Ready
// router status alone does not distinguish.
func (a *routerActor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	status := a.status()
	return map[string]string{
		"router:last_error":             runtime.ErrorText(status.err),
		"router:primary:last_error":     runtime.ErrorText(status.primary.err),
		"router:candidate:last_error":   runtime.ErrorText(status.candidate.err),
		"router:lifecycle":              string(status.lifecycle),
		"router:availability":           string(status.availability),
		"router:revision":               fmt.Sprintf("%d", status.revision),
		"router:normal_routable":        fmt.Sprintf("%t", status.normalRoutable),
		"router:shadow_routable":        fmt.Sprintf("%t", status.shadowRoutable),
		"router:routes":                 fmt.Sprintf("%d", len(a.routesByKey)),
		"router:in_flight_calls":        fmt.Sprintf("%d", len(a.inFlightCalls)),
		"router:primary:lifecycle":      string(status.primary.lifecycle),
		"router:primary:availability":   string(status.primary.availability),
		"router:candidate:lifecycle":    string(status.candidate.lifecycle),
		"router:candidate:availability": string(status.candidate.availability),
	}
}

// isDraining reports whether the router has closed admission for shutdown.
func (a *routerActor[T]) isDraining() bool {
	return a.lifecycle == RouterActorDraining || a.lifecycle == RouterActorStopped
}

// sameRouterActorStatus reports whether two router statuses are equal, for publish deduplication.
func sameRouterActorStatus(left, right routerActorStatus) bool {
	return left.lifecycle == right.lifecycle &&
		left.availability == right.availability &&
		runtime.ErrorText(left.err) == runtime.ErrorText(right.err) &&
		left.revision == right.revision &&
		left.normalRoutable == right.normalRoutable &&
		left.shadowRoutable == right.shadowRoutable &&
		sameDeploymentRouteStatus(left.primary, right.primary) &&
		sameDeploymentRouteStatus(left.candidate, right.candidate)
}

// sameDesiredState reports whether two resolved catalogs ask for the same thing, keyed by plugin id.
func sameDesiredState(left, right map[string]routerDesiredState) bool {
	if len(left) != len(right) {
		return false
	}
	for id, desired := range left {
		other, ok := right[id]
		if !ok || !sameRouterDesiredState(desired, other) {
			return false
		}
	}
	return true
}

// sameRouterDesiredState reports whether one router is being asked for the same thing twice.
func sameRouterDesiredState(left, right routerDesiredState) bool {
	return left.primaryDeferred == right.primaryDeferred &&
		left.candidateDeferred == right.candidateDeferred &&
		sameDeployment(left.primary, right.primary) &&
		sameDeployment(left.candidate, right.candidate)
}

// deploymentRouteName derives a stable, collision-resistant route atom from a route key.
func deploymentRouteName(key DeploymentRouteKey) (gen.Atom, error) {
	encoded, err := json.Marshal(key)
	if err != nil {
		return "", fmt.Errorf("marshal deployment route key: %w", err)
	}
	digest := sha256.Sum256(encoded)
	name := gen.Atom("deployment:" + hex.EncodeToString(digest[:]))
	if len(name) >= 255 {
		return "", fmt.Errorf("deployment route name too long")
	}
	return name, nil
}

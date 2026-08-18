package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// routerState tracks one router actor and its restart state.
type routerState struct {
	pid        gen.PID
	generation uint64
	lastEpoch  uint64
	restart    *runtime.ScheduledBackoff
	status     RouterStatus
	retiring   bool
}

// RouterLifecycle describes one logical-plugin router actor incarnation.
type RouterLifecycle string

const (
	RouterStarting   RouterLifecycle = "starting"
	RouterRunning    RouterLifecycle = "running"
	RouterRestarting RouterLifecycle = "restarting"
	RouterDraining   RouterLifecycle = "draining"
	RouterStopped    RouterLifecycle = "stopped"
)

// RouterStatus is the catalog-facing router status contract.
type RouterStatus struct {
	Lifecycle      RouterLifecycle
	Availability   runtime.Availability
	Generation     uint64
	LastError      error
	Revision       uint64
	NormalRoutable bool
	ShadowRoutable bool
	Primary        DeploymentPoolStatus
	Candidate      DeploymentPoolStatus
}

// clone deep-copies the nested pool statuses so a receiver cannot mutate router state.
func (s RouterStatus) clone() RouterStatus {
	clone := s
	clone.Primary = s.Primary.clone()
	clone.Candidate = s.Candidate.clone()
	return clone
}

// ---------------------------------------------------------------------------
// Route state
// ---------------------------------------------------------------------------

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
	key        DeploymentPoolKey
	deployment Deployment
	name       gen.Atom
	pid        gen.PID
	restart    *runtime.ScheduledBackoff
	status     DeploymentManagerStatus
	phase      deploymentRoutePhase
	managers   map[gen.PID]struct{}
}

// ---------------------------------------------------------------------------
// Router actor
// ---------------------------------------------------------------------------

// routerActor owns one dynamic Ergo route per concrete DeploymentPoolKey.
type routerActor[T Syncable] struct {
	act.Router
	opts             RouterOptions[T]
	pluginID         string
	generation       uint64
	desiredRevision  uint64
	activated        bool
	draining         bool
	drainReported    bool
	everRoutable     bool
	routesByKey      map[DeploymentPoolKey]*deploymentRouteState
	routesByName     map[gen.Atom]DeploymentPoolKey
	inFlightCalls    map[uint64]*routerInvocation
	desiredPrimary   *Deployment
	desiredCandidate *Deployment
	activePrimary    *Deployment
	activeCandidate  *Deployment
	liveStatus       RouterStatus
	statusEpoch      uint64
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
	key     DeploymentPoolKey
	manager gen.PID
}

// MessageApplyRouterDesiredState delivers the desired primary/candidate deployments to apply.
type MessageApplyRouterDesiredState struct {
	desiredRevision   uint64
	primary           *Deployment
	candidate         *Deployment
	primaryDeferred   bool
	candidateDeferred bool
}

// MessageRouterDrained reports to the catalog that the router has fully drained.
type MessageRouterDrained struct {
	pluginID   string
	pid        gen.PID
	generation uint64
}

// MessageRouterStatusChanged publishes an epoch-ordered router status to the catalog.
type MessageRouterStatusChanged struct {
	pluginID   string
	pid        gen.PID
	generation uint64
	epoch      uint64
	status     RouterStatus
}

// MessageRetryRouteStep is the self-timer that re-drives a route's pending lifecycle step.
type MessageRetryRouteStep struct {
	route gen.Atom
	token uint64
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init allocates the router's route and in-flight-call indexes.
func (a *routerActor[T]) Init(...any) (act.RouterOptions, error) {
	a.opts = routerOptionsWithDefaults(a.opts)
	a.routesByKey = make(map[DeploymentPoolKey]*deploymentRouteState)
	a.routesByName = make(map[gen.Atom]DeploymentPoolKey)
	a.inFlightCalls = make(map[uint64]*routerInvocation)
	return act.RouterOptions{}, nil
}

// Terminate cancels pending timers for in-flight calls and route restarts.
func (a *routerActor[T]) Terminate(error) {
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

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

// RouteMessage is the only normal-priority ingress path. Timers are normal too,
// so their expiration is consumed here rather than forwarded to a route.
func (a *routerActor[T]) RouteMessage(_ gen.PID, message any) gen.Atom {
	switch m := message.(type) {
	case MessageInvocationTimedOut:
		call := a.inFlightCalls[m.callID]
		if call != nil && !call.accepted && call.ackToken == m.token {
			a.finishTrackedCall(m.callID, runtime.ErrPluginUnavailable)
		}
		return act.RouteDiscard
	case MessageRetryRouteStep:
		a.retryRouteStep(m)
		return act.RouteDiscard
	case MessageInvokePlugin[T]:
		return a.routeInvocation(m)
	default:
		return act.RouteDiscard
	}
}

// RouteCall discards synchronous calls; the router serves none over the route path.
func (a *routerActor[T]) RouteCall(_ gen.PID, _ gen.Ref, _ any) gen.Atom { return act.RouteDiscard }

// HandleMessage receives all router administration and route lifecycle facts at
// High/Max priority, plus Ergo's routed-send failure sentinel.
func (a *routerActor[T]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageRouterActivate:
		if m.generation <= a.generation {
			return nil
		}
		if a.activated {
			return fmt.Errorf("router %q already activated as generation %d", a.pluginID, a.generation)
		}
		a.generation, a.activated = m.generation, true
		a.reconcileStatus()

	case MessageApplyRouterDesiredState:
		if !a.activated || a.draining || m.desiredRevision < a.desiredRevision {
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
		a.finishTrackedCall(call.CallID, runtime.ErrPluginUnavailable)
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
	return fmt.Errorf("actorruntime: unsupported router call %T", request), nil
}

// ---------------------------------------------------------------------------
// Desired-state reconciliation
// ---------------------------------------------------------------------------

// applyDeployment creates or updates the route backing one desired deployment.
func (a *routerActor[T]) applyDeployment(d *Deployment) error {
	if d == nil || a.draining {
		return nil
	}
	key := d.PoolKey()
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
	keep := make(map[DeploymentPoolKey]bool)
	for _, deployment := range []*Deployment{a.desiredPrimary, a.desiredCandidate, a.activePrimary, a.activeCandidate} {
		if deployment != nil {
			keep[deployment.PoolKey()] = true
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
	ref := a.routesByKey[desired.PoolKey()]
	if ref != nil && ref.phase == deploymentRouteActive &&
		ref.status.Lifecycle == DeploymentManagerRunning && ref.status.Availability == runtime.AvailabilityReady {
		*active = desired
	}
}

// ---------------------------------------------------------------------------
// Route lifecycle
// ---------------------------------------------------------------------------

// addRoute registers the dynamic route and schedules a retry step if registration fails.
func (a *routerActor[T]) addRoute(ref *deploymentRouteState) error {
	err := a.AddRoute(act.Route{Name: ref.name, Factory: func() gen.ProcessBehavior {
		return a.newDeploymentManager(ref)
	}})
	if err != nil {
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
	} else if a.draining || ref.phase == deploymentRouteDraining {
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
			_ = a.scheduleRouteStep(ref)
		}
	}
	a.reconcileStatus()
}

// scheduleRouteStep arms a per-route backoff timer that re-drives its pending lifecycle step.
func (a *routerActor[T]) scheduleRouteStep(ref *deploymentRouteState) error {
	if a.draining && ref.phase < deploymentRouteDraining {
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
		return fmt.Errorf("deployment route step for %v: %w", ref.key, runtime.ErrBackoffStopped)
	}
	ref.restart.Token++
	cancel, err := a.SendAfter(a.PID(), MessageRetryRouteStep{route: ref.name, token: ref.restart.Token}, delay)
	if err != nil {
		return fmt.Errorf("schedule deployment route step: %w", err)
	}
	ref.restart.Pending, ref.restart.Cancel = true, cancel
	return nil
}

// newDeploymentManager builds the DeploymentManager child that serves this route.
func (a *routerActor[T]) newDeploymentManager(ref *deploymentRouteState) *DeploymentManager[T] {
	ref.status = DeploymentManagerStatus{
		Lifecycle:    DeploymentManagerStarting,
		Availability: runtime.AvailabilityUnavailable,
		Workers:      make(map[gen.PID]DeploymentWorkerStatus),
	}
	return &DeploymentManager[T]{
		adapter:    a.opts.Adapter,
		options:    a.opts.ManagerOptions,
		deployment: ref.deployment,
		route:      ref.name,
		draining:   ref.phase >= deploymentRouteDraining || a.draining,
	}
}

// refreshRoutePID reads the route's live manager PID and records it in the fencing set.
func (a *routerActor[T]) refreshRoutePID(ref *deploymentRouteState) gen.PID {
	info, ok := a.Route(ref.name)
	if !ok {
		return gen.PID{}
	}
	ref.pid = info.PID
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

// ---------------------------------------------------------------------------
// Invocation routing
// ---------------------------------------------------------------------------

// routeInvocation selects the rollout target route and tracks the call awaiting acceptance.
func (a *routerActor[T]) routeInvocation(call MessageInvokePlugin[T]) gen.Atom {
	if a.draining {
		_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: runtime.ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return act.RouteDiscard
	}
	// Rollout routing decision: shadow -> shadow candidate; else primary,
	// unless a canary candidate wins this call's rollout bucket.
	var ref *deploymentRouteState
	if call.Shadow {
		if a.activeCandidate != nil && a.activeCandidate.Mode == runtime.RolloutModeShadow {
			ref = a.routesByKey[a.activeCandidate.PoolKey()]
		}
	} else {
		target := a.activePrimary
		if a.activeCandidate != nil && a.activeCandidate.Mode == runtime.RolloutModeCanary &&
			float64(runtime.RolloutBucket(call.RolloutKey)) <= a.activeCandidate.RolloutPct {
			target = a.activeCandidate
		}
		if target != nil {
			ref = a.routesByKey[target.PoolKey()]
		}
	}
	if ref == nil || ref.phase != deploymentRouteActive || a.refreshRoutePID(ref) == (gen.PID{}) {
		_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: runtime.ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return act.RouteDiscard
	}
	if _, exists := a.inFlightCalls[call.CallID]; exists {
		return act.RouteDiscard
	}
	tracked := &routerInvocation{route: ref.name, ackToken: 1}
	timeout := a.opts.ManagerOptions.DispatchTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cancel, err := a.SendAfter(a.PID(), MessageInvocationTimedOut{callID: call.CallID, token: tracked.ackToken}, timeout)
	if err != nil {
		_ = a.SendWithPriority(a.Parent(), MessageInvocationCompleted{CallID: call.CallID, Err: runtime.ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return act.RouteDiscard
	}
	tracked.ackStop = cancel
	a.inFlightCalls[call.CallID] = tracked
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

// ---------------------------------------------------------------------------
// Manager failure
// ---------------------------------------------------------------------------

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
	for callID, call := range a.inFlightCalls {
		if call.accepted && call.route == message.route && call.manager == message.manager {
			a.finishTrackedCall(callID, runtime.ErrPluginUnavailable)
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
// Draining
// ---------------------------------------------------------------------------

// drainRoute advances one route toward teardown: quiesce its manager, then let it be removed.
func (a *routerActor[T]) drainRoute(ref *deploymentRouteState) {
	switch ref.phase {
	case deploymentRoutePending:
		if ref.restart != nil {
			ref.restart.CancelScheduled(false)
		}
		delete(a.routesByKey, ref.key)
		delete(a.routesByName, ref.name)
		if a.activePrimary != nil && a.activePrimary.PoolKey() == ref.key {
			a.activePrimary = nil
		}
		if a.activeCandidate != nil && a.activeCandidate.PoolKey() == ref.key {
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
	// No live manager: respawn one (it starts already draining, per
	// newDeploymentManager) so it can run the drain protocol to completion.
	if err := a.RespawnRoute(ref.name); err != nil {
		_ = a.scheduleRouteStep(ref)
		return
	}
	// Respawn may not have registered the PID yet; drain it if it is live,
	// otherwise reschedule to retry the drain once it appears.
	if pid := a.refreshRoutePID(ref); pid != (gen.PID{}) {
		_ = a.Send(pid, MessageDrain{})
		return
	}
	_ = a.scheduleRouteStep(ref)
}

// beginDrain starts a global drain of every route and reports if it is already complete.
func (a *routerActor[T]) beginDrain() {
	if a.draining {
		return
	}
	a.draining = true
	for _, ref := range a.routesByKey {
		a.drainRoute(ref)
	}
	a.reconcileStatus()
	a.reportDrained()
}

// reportDrained announces router drain completion to the catalog once no routes remain.
func (a *routerActor[T]) reportDrained() {
	if !a.draining || len(a.routesByKey) > 0 || a.drainReported {
		return
	}
	a.drainReported = true
	a.reconcileStatus()
	_ = a.SendWithPriority(a.Parent(), MessageRouterDrained{pluginID: a.pluginID, pid: a.PID(), generation: a.generation}, gen.MessagePriorityHigh)
}

// removeDrainedRoute unregisters a drained route and clears any active pointer to it.
func (a *routerActor[T]) removeDrainedRoute(ref *deploymentRouteState) {
	ref.phase = deploymentRouteRemoving
	if err := a.RemoveRoute(ref.name); err != nil {
		_ = a.scheduleRouteStep(ref)
		return
	}
	delete(a.routesByKey, ref.key)
	delete(a.routesByName, ref.name)
	if a.activePrimary != nil && a.activePrimary.PoolKey() == ref.key {
		a.activePrimary = nil
	}
	if a.activeCandidate != nil && a.activeCandidate.PoolKey() == ref.key {
		a.activeCandidate = nil
	}
}

// ---------------------------------------------------------------------------
// Status projection
// ---------------------------------------------------------------------------

// deploymentStatusFor projects a deployment's route into the catalog-facing pool status.
func (a *routerActor[T]) deploymentStatusFor(deployment *Deployment) DeploymentPoolStatus {
	if deployment == nil {
		return DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[gen.PID]DeploymentWorkerStatus),
		}
	}
	ref := a.routesByKey[deployment.PoolKey()]
	if ref == nil {
		return DeploymentPoolStatus{
			Lifecycle:      DeploymentPoolStarting,
			Availability:   runtime.AvailabilityUnavailable,
			DesiredWorkers: deployment.WorkerCount(),
			Workers:        make(map[gen.PID]DeploymentWorkerStatus),
		}
	}
	lifecycle := DeploymentPoolStarting
	switch ref.status.Lifecycle {
	case DeploymentManagerRunning:
		lifecycle = DeploymentPoolRunning
	case DeploymentManagerDraining:
		lifecycle = DeploymentPoolDraining
	case DeploymentManagerStopped:
		lifecycle = DeploymentPoolStopped
	case DeploymentManagerFailed:
		lifecycle = DeploymentPoolFailed
	}
	if ref.restart != nil && ref.restart.Pending {
		lifecycle = DeploymentPoolRestarting
	}
	return DeploymentPoolStatus{
		Lifecycle:      lifecycle,
		Availability:   ref.status.Availability,
		HealthyWorkers: ref.status.ReadyWorkers,
		DesiredWorkers: ref.status.CurrentProcs,
		QueueDepth:     ref.status.QueueDepth,
		ActiveCalls:    ref.status.Active + ref.status.Dispatching,
		Workers:        ref.status.Workers,
	}
}

// activeRouteAvailable reports whether a deployment's route is active with a live manager.
func (a *routerActor[T]) activeRouteAvailable(deployment *Deployment) bool {
	if deployment == nil {
		return false
	}
	ref := a.routesByKey[deployment.PoolKey()]
	return ref != nil && ref.phase == deploymentRouteActive && a.refreshRoutePID(ref) != (gen.PID{})
}

// reconcileStatus recomputes router status and publishes an epoch-tagged update on change.
func (a *routerActor[T]) reconcileStatus() {
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
	if normalRoutable {
		a.everRoutable = true
	}
	primaryHealthy := a.desiredPrimary == nil || (primaryStatus.Lifecycle == DeploymentPoolRunning && primaryStatus.Availability == runtime.AvailabilityReady)
	candidateHealthy := a.desiredCandidate == nil || (candidateStatus.Lifecycle == DeploymentPoolRunning && candidateStatus.Availability == runtime.AvailabilityReady)
	lifecycle := RouterStarting
	switch {
	case a.drainReported:
		lifecycle = RouterStopped
	case a.draining:
		lifecycle = RouterDraining
	case a.everRoutable:
		lifecycle = RouterRunning
	}
	availability := runtime.AvailabilityUnavailable
	switch {
	case fullNormalCoverage && primaryHealthy && candidateHealthy:
		availability = runtime.AvailabilityReady
	case normalRoutable || shadowRoutable:
		availability = runtime.AvailabilityDegraded
	}
	next := RouterStatus{Lifecycle: lifecycle, Availability: availability, Generation: a.generation, Revision: a.desiredRevision,
		NormalRoutable: normalRoutable, ShadowRoutable: shadowRoutable, Primary: primaryStatus, Candidate: candidateStatus}
	if sameRouterStatus(a.liveStatus, next) && a.statusEpoch != 0 {
		return
	}
	a.statusEpoch, a.liveStatus = a.statusEpoch+1, next
	if a.activated {
		_ = a.SendWithPriority(a.Parent(), MessageRouterStatusChanged{pluginID: a.pluginID, pid: a.PID(), generation: a.generation, epoch: a.statusEpoch, status: next.clone()}, gen.MessagePriorityHigh)
	}
}

// sameRouterStatus reports whether two router statuses are equal, for publish deduplication.
func sameRouterStatus(left, right RouterStatus) bool {
	return left.Lifecycle == right.Lifecycle &&
		left.Availability == right.Availability &&
		left.Generation == right.Generation &&
		errorText(left.LastError) == errorText(right.LastError) &&
		left.Revision == right.Revision &&
		left.NormalRoutable == right.NormalRoutable &&
		left.ShadowRoutable == right.ShadowRoutable &&
		sameDeploymentPoolStatus(left.Primary, right.Primary) &&
		sameDeploymentPoolStatus(left.Candidate, right.Candidate)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// deploymentRouteName derives a stable, collision-resistant route atom from a pool key.
func deploymentRouteName(key DeploymentPoolKey) (gen.Atom, error) {
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

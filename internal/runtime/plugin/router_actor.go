package plugin

import (
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/runtime"
)

// RouterLifecycle describes one logical-plugin router actor incarnation.
type RouterLifecycle string

const (
	RouterStarting   RouterLifecycle = "starting"
	RouterRunning    RouterLifecycle = "running"
	RouterRestarting RouterLifecycle = "restarting"
	RouterDraining   RouterLifecycle = "draining"
	RouterStopped    RouterLifecycle = "stopped"
)

// RouterStatus is owned by routerActor, except for RestartCount and
// ActorLastError, which are owned by catalogActor because the catalog replaces
// router actor incarnations.
type RouterStatus struct {
	Lifecycle       RouterLifecycle
	Availability    runtime.Availability
	ActorGeneration uint64
	RestartCount    uint64
	RestartPending  bool
	ActorLastError  string
	Revision        uint64
	NormalRoutable  bool
	ShadowRoutable  bool
	Primary         DeploymentPoolStatus
	Candidate       DeploymentPoolStatus
}

func (s RouterStatus) clone() RouterStatus {
	clone := s
	clone.Primary = s.Primary.clone()
	clone.Candidate = s.Candidate.clone()
	return clone
}

// mergeDeploymentPoolStatus turns a child-emitted live status snapshot into the
// authoritative router-owned status for that deployment-pool incarnation.
func mergeDeploymentPoolStatus(child DeploymentPoolStatus, actorGeneration uint64, restartCount uint64, restartPending bool, actorLastError string) DeploymentPoolStatus {
	status := child.clone()
	status.ActorGeneration = actorGeneration
	status.RestartCount = restartCount
	status.RestartPending = restartPending
	status.ActorLastError = actorLastError
	return status
}

type deploymentPoolState struct {
	pid             gen.PID
	actorGeneration uint64
	lastEpoch       uint64
	restartCount    uint64
	lastError       string
	status          DeploymentPoolStatus
	restart         *runtime.ScheduledBackoff
	draining        bool
}

type routerActor[T plugin.Syncable] struct {
	act.Actor

	deps     runtime.ActorDependencies[T]
	pluginID string

	actorGeneration uint64
	activated       bool
	everRoutable    bool

	pools         map[runtime.DeploymentPoolKey]*deploymentPoolState
	inFlightCalls map[uint64]gen.PID

	desiredPrimary   *runtime.Deployment
	desiredCandidate *runtime.Deployment
	activePrimary    *runtime.Deployment
	activeCandidate  *runtime.Deployment
	desiredRevision  uint64

	liveStatus  RouterStatus
	statusEpoch uint64

	draining      bool
	drainReported bool
}

type MessageRouterActivate struct{ generation uint64 }

type MessageApplyRouterDesiredState struct {
	desiredRevision   uint64
	primary           *runtime.Deployment
	candidate         *runtime.Deployment
	primaryDeferred   bool
	candidateDeferred bool
}

type MessageRouterDrained struct {
	pluginID   string
	pid        gen.PID
	generation uint64
}

type MessageRouterStatusChanged struct {
	pluginID   string
	pid        gen.PID
	generation uint64
	epoch      uint64
	status     RouterStatus
}

type MessageDeploymentPoolRestart struct {
	poolKey         runtime.DeploymentPoolKey
	desiredRevision uint64
	token           uint64
}

func (a *routerActor[T]) Init(...any) error {
	if a.pools == nil {
		a.pools = make(map[runtime.DeploymentPoolKey]*deploymentPoolState)
	}
	if a.inFlightCalls == nil {
		a.inFlightCalls = make(map[uint64]gen.PID)
	}
	return nil
}

func (a *routerActor[T]) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case MessageRouterActivate:
		if m.generation <= a.actorGeneration {
			return nil
		}
		if a.activated {
			return fmt.Errorf(
				"router %q already activated as generation %d",
				a.pluginID,
				a.actorGeneration,
			)
		}
		a.actorGeneration = m.generation
		a.activated = true
		a.reconcileStatus()

	case MessageApplyRouterDesiredState:
		if !a.activated || a.draining || m.desiredRevision < a.desiredRevision {
			return nil
		}

		if err := a.validateDesiredDeployment(m.primary); err != nil {
			return err
		}

		if err := a.validateDesiredDeployment(m.candidate); err != nil {
			return err
		}

		a.desiredRevision = m.desiredRevision
		if !m.primaryDeferred {
			a.desiredPrimary = m.primary
			a.startDeploymentPool(m.primary)
		}
		if !m.candidateDeferred {
			a.desiredCandidate = m.candidate
			a.startDeploymentPool(m.candidate)
		}
		a.reconcileDeployments()
		a.reconcileStatus()

	case runtime.MessageInvokePlugin[T]:
		a.routeCall(m)

	case runtime.MessageCancelInvocation:
		poolPID, ok := a.inFlightCalls[m.CallID]
		if !ok {
			return nil
		}
		if err := a.Send(poolPID, m); err != nil {
			a.finishTrackedCall(m.CallID, m.Err)
		}

	case runtime.MessageInvocationCompleted:
		if _, ok := a.inFlightCalls[m.CallID]; ok {
			delete(a.inFlightCalls, m.CallID)
			_ = a.Send(a.Parent(), m)
		}

	case MessageDeploymentPoolStatusChanged:
		ref := a.pools[m.poolKey]
		if ref == nil ||
			ref.pid != m.pid ||
			ref.actorGeneration != m.actorGeneration ||
			m.epoch <= ref.lastEpoch {
			return nil
		}
		ref.lastEpoch = m.epoch

		ref.lastError = ""
		a.resetDeploymentPoolRestartBackoff(m.poolKey)
		ref.status = mergeDeploymentPoolStatus(
			m.status,
			ref.actorGeneration,
			ref.restartCount,
			a.deploymentPoolRestartPending(m.poolKey),
			ref.lastError,
		)
		next := ref.status
		ref.draining = next.Lifecycle == DeploymentPoolDraining ||
			next.Lifecycle == DeploymentPoolStopped

		a.reconcileDeployments()
		a.reconcileStatus()

	case MessageDeploymentPoolRestart:
		ref := a.pools[m.poolKey]
		if ref == nil || ref.restart == nil || !ref.restart.Pending || ref.restart.Token != m.token {
			return nil
		}

		ref.restart.Pending = false
		ref.restart.Cancel = nil
		ref.status.RestartPending = false

		if !a.draining && m.desiredRevision == a.desiredRevision {
			a.startDeploymentPool(a.desiredDeployment(m.poolKey))
			a.reconcileDeployments()
			a.reconcileStatus()
		}

	case runtime.MessageDrain:
		if a.draining {
			return nil
		}
		a.draining = true
		a.cancelAllDeploymentPoolRestarts(false)
		a.reconcileStatus()
		if a.liveDeploymentPoolCount() == 0 {
			a.reportDrained()
			return nil
		}
		for _, ref := range a.pools {
			if ref.pid == (gen.PID{}) {
				continue
			}
			ref.draining = true
			_ = a.Send(ref.pid, runtime.MessageDrain{})
		}

	case runtime.MessageStop:
		return gen.TerminateReasonNormal

	case MessageDeploymentPoolDrained:
		ref := a.pools[m.poolKey]
		if ref == nil ||
			ref.pid != m.pid ||
			ref.actorGeneration != m.actorGeneration {
			return nil
		}

		_ = a.Send(m.pid, runtime.MessageStop{})
		a.retireDeploymentPool(m.poolKey, runtime.ErrPluginUnavailable)

		if a.draining {
			a.reconcileStatus()
			if a.liveDeploymentPoolCount() == 0 {
				a.reportDrained()
			}
			return nil
		}

		a.startDeploymentPool(a.desiredPrimary)
		a.startDeploymentPool(a.desiredCandidate)
		a.reconcileDeployments()
		a.reconcileStatus()

	case gen.MessageDownPID:
		for poolKey, ref := range a.pools {
			if ref.pid != m.PID {
				continue
			}

			ref.restartCount++
			ref.lastError = errorText(m.Reason)
			desired := a.desiredDeployment(poolKey)
			shouldRestart := !a.draining && !ref.draining && desired != nil

			if !shouldRestart {
				a.retireDeploymentPool(poolKey, runtime.ErrPluginUnavailable)
				if a.draining && a.liveDeploymentPoolCount() == 0 {
					a.reportDrained()
				}
			} else {
				for callID, poolPID := range a.inFlightCalls {
					if poolPID == m.PID {
						a.finishTrackedCall(callID, runtime.ErrPluginUnavailable)
					}
				}

				ref.pid = gen.PID{}
				ref.lastEpoch = 0
				ref.status.Lifecycle = DeploymentPoolRestarting
				ref.status.Availability = runtime.AvailabilityUnavailable
				ref.status.ActorGeneration = ref.actorGeneration
				ref.status.RestartCount = ref.restartCount
				ref.status.RestartPending = false
				ref.status.ActorLastError = ref.lastError

				if a.activePrimary != nil && a.activePrimary.PoolKey() == poolKey {
					a.activePrimary = nil
				}
				if a.activeCandidate != nil && a.activeCandidate.PoolKey() == poolKey {
					a.activeCandidate = nil
				}

				_ = a.scheduleDeploymentPoolRestart(poolKey)
			}
			a.reconcileStatus()
			break
		}
	}
	return nil
}

func (a *routerActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("actorruntime: unsupported router call %T", request)
}

func (a *routerActor[T]) startDeploymentPool(d *runtime.Deployment) {
	if d == nil || a.draining {
		return
	}
	poolKey := d.PoolKey()
	ref := a.pools[poolKey]
	if ref != nil && ref.pid != (gen.PID{}) {
		return
	}
	if ref == nil {
		ref = &deploymentPoolState{}
		a.pools[poolKey] = ref
	}

	ref.actorGeneration++
	generation := ref.actorGeneration
	ref.lastEpoch = 0
	ref.draining = false
	ref.status = DeploymentPoolStatus{
		Lifecycle:       DeploymentPoolStarting,
		Availability:    runtime.AvailabilityUnavailable,
		ActorGeneration: generation,
		RestartCount:    ref.restartCount,
		ActorLastError:  ref.lastError,
		DesiredWorkers:  d.WorkerCount(),
		Workers:         make(map[int]PluginWorkerStatus),
	}
	a.reconcileStatus()

	pid, err := a.Spawn(func() gen.ProcessBehavior {
		return &deploymentPoolActor[T]{
			deps:       a.deps,
			deployment: *d,
		}
	}, gen.ProcessOptions{LinkParent: true})
	if err != nil {
		ref.restartCount++
		ref.status.RestartCount = ref.restartCount
		ref.status.Lifecycle = DeploymentPoolRestarting
		ref.lastError = fmt.Sprintf("spawn deployment pool: %v", err)
		ref.status.ActorLastError = ref.lastError
		_ = a.scheduleDeploymentPoolRestart(poolKey)
		a.reconcileStatus()
		return
	}

	ref.pid = pid
	if err := a.MonitorPID(pid); err != nil {
		_ = a.Node().SendExit(pid, gen.TerminateReasonShutdown)
		ref.pid = gen.PID{}
		ref.restartCount++
		ref.status.RestartCount = ref.restartCount
		ref.lastError = fmt.Sprintf("monitor deployment pool: %v", err)
		ref.status.Lifecycle = DeploymentPoolRestarting
		ref.status.ActorLastError = ref.lastError
		_ = a.scheduleDeploymentPoolRestart(poolKey)
		a.reconcileStatus()
		return
	}

	if err := a.Send(pid, MessageDeploymentPoolActivate{generation: generation}); err != nil {
		_ = a.Node().SendExit(pid, fmt.Errorf("activate deployment pool: %w", err))
	}
}

func (a *routerActor[T]) stopDeploymentPool(poolKey runtime.DeploymentPoolKey, reason error) {
	ref := a.pools[poolKey]
	if ref == nil || ref.pid == (gen.PID{}) {
		return
	}
	_ = a.Node().SendExit(ref.pid, reason)
}

func (a *routerActor[T]) retireDeploymentPool(poolKey runtime.DeploymentPoolKey, callErr error) {
	ref := a.pools[poolKey]
	if ref == nil {
		return
	}

	if ref.restart != nil {
		ref.restart.CancelScheduled(false)
	}

	poolPID := ref.pid
	if poolPID != (gen.PID{}) {
		for callID, targetPID := range a.inFlightCalls {
			if targetPID == poolPID {
				a.finishTrackedCall(callID, callErr)
			}
		}
	}

	ref.pid = gen.PID{}
	ref.lastEpoch = 0
	ref.draining = false

	ref.status.Lifecycle = DeploymentPoolStopped
	ref.status.Availability = runtime.AvailabilityUnavailable
	ref.status.ActorGeneration = ref.actorGeneration
	ref.status.RestartCount = ref.restartCount
	ref.status.RestartPending = false
	ref.status.ActorLastError = ref.lastError
	ref.status.HealthyWorkers = 0
	ref.status.QueueDepth = 0
	ref.status.ActiveCalls = 0
	ref.status.Workers = make(map[int]PluginWorkerStatus)

	if a.activePrimary != nil && a.activePrimary.PoolKey() == poolKey {
		a.activePrimary = nil
	}
	if a.activeCandidate != nil && a.activeCandidate.PoolKey() == poolKey {
		a.activeCandidate = nil
	}
}

func (a *routerActor[T]) deploymentPoolRestartState(poolKey runtime.DeploymentPoolKey) *runtime.ScheduledBackoff {
	ref := a.pools[poolKey]
	if ref == nil {
		ref = &deploymentPoolState{}
		a.pools[poolKey] = ref
	}

	if ref.restart == nil {
		ref.restart = runtime.NewScheduledBackoff(a.deps.RetryMin, a.deps.RetryMax)
	}

	return ref.restart
}

func (a *routerActor[T]) scheduleDeploymentPoolRestart(poolKey runtime.DeploymentPoolKey) error {
	if a.draining {
		return nil
	}
	ref := a.pools[poolKey]
	if ref == nil {
		ref = &deploymentPoolState{}
		a.pools[poolKey] = ref
	}
	state := a.deploymentPoolRestartState(poolKey)
	if state.Pending {
		return nil
	}

	delay := state.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("deployment pool restart backoff stopped for %v", poolKey)
	}
	state.Token++
	token := state.Token
	cancel, err := a.SendAfter(
		a.PID(),
		MessageDeploymentPoolRestart{poolKey: poolKey, desiredRevision: a.desiredRevision, token: token},
		delay,
	)
	if err != nil {
		return fmt.Errorf("schedule deployment pool restart for %v: %w", poolKey, err)
	}
	state.Pending = true
	state.Cancel = cancel
	ref.status.Lifecycle = DeploymentPoolRestarting
	ref.status.Availability = runtime.AvailabilityUnavailable
	ref.status.RestartPending = true
	ref.status.ActorLastError = ref.lastError
	a.reconcileStatus()
	return nil
}

func (a *routerActor[T]) cancelDeploymentPoolRestart(poolKey runtime.DeploymentPoolKey, reset bool) {
	ref := a.pools[poolKey]
	if ref == nil {
		return
	}
	if ref.restart != nil {
		ref.restart.CancelScheduled(reset)
	}
	ref.status.RestartPending = false
}

func (a *routerActor[T]) resetDeploymentPoolRestartBackoff(poolKey runtime.DeploymentPoolKey) {
	a.cancelDeploymentPoolRestart(poolKey, true)
}

func (a *routerActor[T]) cancelAllDeploymentPoolRestarts(reset bool) {
	for poolKey := range a.pools {
		a.cancelDeploymentPoolRestart(poolKey, reset)
	}
}

func (a *routerActor[T]) deploymentPoolRestartPending(poolKey runtime.DeploymentPoolKey) bool {
	ref := a.pools[poolKey]
	return ref != nil && ref.restart != nil && ref.restart.Pending
}

func (a *routerActor[T]) validateDesiredDeployment(d *runtime.Deployment) error {
	if d == nil || d.Id == a.pluginID {
		return nil
	}
	return fmt.Errorf("deployment %q belongs to plugin %q, router owns %q", d.Name, d.Id, a.pluginID)
}

func (a *routerActor[T]) desiredDeployment(poolKey runtime.DeploymentPoolKey) *runtime.Deployment {
	if a.desiredPrimary != nil && a.desiredPrimary.PoolKey() == poolKey {
		return a.desiredPrimary
	}
	if a.desiredCandidate != nil && a.desiredCandidate.PoolKey() == poolKey {
		return a.desiredCandidate
	}
	return nil
}

func (a *routerActor[T]) reconcileDeployments() {
	if a.desiredPrimary == nil {
		a.activePrimary = nil
	} else if ref := a.pools[a.desiredPrimary.PoolKey()]; ref != nil && ref.status.routable() && !ref.draining {
		a.activePrimary = a.desiredPrimary
	}

	if a.desiredCandidate == nil {
		a.activeCandidate = nil
	} else if ref := a.pools[a.desiredCandidate.PoolKey()]; ref != nil && ref.status.routable() && !ref.draining {
		a.activeCandidate = a.desiredCandidate
	}

	a.drainObsoleteDeploymentPools()
}

func (a *routerActor[T]) drainObsoleteDeploymentPools() {
	keep := make(map[runtime.DeploymentPoolKey]bool)
	if a.activePrimary != nil {
		keep[a.activePrimary.PoolKey()] = true
	}
	if a.desiredPrimary != nil {
		keep[a.desiredPrimary.PoolKey()] = true
	}
	if a.desiredCandidate != nil {
		keep[a.desiredCandidate.PoolKey()] = true
	}
	if a.activeCandidate != nil {
		keep[a.activeCandidate.PoolKey()] = true
	}

	for key, ref := range a.pools {
		if keep[key] || ref.draining {
			continue
		}
		if ref.pid == (gen.PID{}) {
			if ref.status.Lifecycle != DeploymentPoolStopped {
				a.retireDeploymentPool(key, runtime.ErrPluginUnavailable)
			}
			continue
		}

		ref.draining = true
		a.cancelDeploymentPoolRestart(key, false)
		_ = a.Send(ref.pid, runtime.MessageDrain{})
	}
}

func (a *routerActor[T]) routeCall(call runtime.MessageInvokePlugin[T]) {
	if a.draining {
		a.finishUntrackedCall(call, runtime.ErrPluginUnavailable)
		return
	}

	if call.Shadow {
		if a.activeCandidate == nil {
			a.finishUntrackedCall(call, nil)
			return
		}
		ref := a.pools[a.activeCandidate.PoolKey()]
		if ref == nil ||
			!ref.status.routable() ||
			ref.draining ||
			a.activeCandidate.Mode != pools.RolloutModeShadow {
			a.finishUntrackedCall(call, nil)
			return
		}
		a.forwardCall(ref.pid, call)
		return
	}

	target := a.activePrimary
	if a.activeCandidate != nil {
		if ref := a.pools[a.activeCandidate.PoolKey()]; ref != nil &&
			ref.status.routable() &&
			!ref.draining &&
			a.activeCandidate.Mode == pools.RolloutModeCanary &&
			float64(pools.RolloutBucket(call.RolloutKey)) <= a.activeCandidate.RolloutPct {
			target = a.activeCandidate
		}
	}

	if target == nil {
		a.finishUntrackedCall(call, runtime.ErrPluginUnavailable)
		return
	}
	ref := a.pools[target.PoolKey()]
	if ref == nil || !ref.status.routable() || ref.draining {
		a.finishUntrackedCall(call, runtime.ErrPluginUnavailable)
		return
	}
	a.forwardCall(ref.pid, call)
}

func (a *routerActor[T]) forwardCall(poolPID gen.PID, call runtime.MessageInvokePlugin[T]) {
	a.inFlightCalls[call.CallID] = poolPID
	if err := a.Send(poolPID, call); err != nil {
		a.finishTrackedCall(call.CallID, runtime.ErrPluginUnavailable)
		_ = a.Node().SendExit(poolPID, fmt.Errorf("forward invocation to deployment pool: %w", err))
	}
}

func (a *routerActor[T]) finishUntrackedCall(call runtime.MessageInvokePlugin[T], err error) {
	_ = a.Send(a.Parent(), runtime.MessageInvocationCompleted{CallID: call.CallID, Err: err})
}

func (a *routerActor[T]) finishTrackedCall(callID uint64, err error) {
	if _, ok := a.inFlightCalls[callID]; !ok {
		return
	}
	delete(a.inFlightCalls, callID)
	_ = a.Send(a.Parent(), runtime.MessageInvocationCompleted{CallID: callID, Err: err})
}

func (a *routerActor[T]) deploymentStatusFor(deployment *runtime.Deployment) DeploymentPoolStatus {
	if deployment == nil {
		return DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[int]PluginWorkerStatus),
		}
	}
	if ref := a.pools[deployment.PoolKey()]; ref != nil {
		status := ref.status.clone()
		status.ActorGeneration = ref.actorGeneration
		status.RestartCount = ref.restartCount
		status.RestartPending = a.deploymentPoolRestartPending(deployment.PoolKey())
		status.ActorLastError = ref.lastError
		return status
	}
	return DeploymentPoolStatus{
		Lifecycle:      DeploymentPoolStarting,
		Availability:   runtime.AvailabilityUnavailable,
		DesiredWorkers: deployment.WorkerCount(),
		Workers:        make(map[int]PluginWorkerStatus),
	}
}

func (a *routerActor[T]) deploymentRoutable(deployment *runtime.Deployment) bool {
	if deployment == nil {
		return false
	}
	ref := a.pools[deployment.PoolKey()]
	return ref != nil && !ref.draining && ref.status.routable()
}

func (a *routerActor[T]) liveDeploymentPoolCount() int {
	count := 0
	for _, ref := range a.pools {
		if ref.pid != (gen.PID{}) {
			count++
		}
	}
	return count
}

func (a *routerActor[T]) reconcileStatus() {
	primaryStatus := a.deploymentStatusFor(a.desiredPrimary)
	candidateStatus := a.deploymentStatusFor(a.desiredCandidate)

	primaryRoutable := a.deploymentRoutable(a.activePrimary)
	candidateRoutable := a.deploymentRoutable(a.activeCandidate)
	shadowRoutable := candidateRoutable &&
		a.activeCandidate != nil &&
		a.activeCandidate.Mode == pools.RolloutModeShadow

	fullNormalCoverage := primaryRoutable
	normalRoutable := primaryRoutable
	if candidateRoutable &&
		a.activeCandidate != nil &&
		a.activeCandidate.Mode == pools.RolloutModeCanary {
		normalRoutable = normalRoutable || a.activeCandidate.RolloutPct > 0
		if a.activePrimary == nil && a.activeCandidate.RolloutPct >= 100 {
			fullNormalCoverage = true
		}
	}
	if normalRoutable {
		a.everRoutable = true
	}

	primaryHealthy := a.desiredPrimary == nil ||
		(primaryStatus.Lifecycle == DeploymentPoolRunning &&
			primaryStatus.Availability == runtime.AvailabilityReady)
	candidateHealthy := a.desiredCandidate == nil ||
		(candidateStatus.Lifecycle == DeploymentPoolRunning &&
			candidateStatus.Availability == runtime.AvailabilityReady)
	dependenciesHealthy := primaryHealthy && candidateHealthy

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
	case fullNormalCoverage && dependenciesHealthy:
		availability = runtime.AvailabilityReady
	case normalRoutable || shadowRoutable:
		availability = runtime.AvailabilityDegraded
	}

	next := RouterStatus{
		Lifecycle:       lifecycle,
		Availability:    availability,
		ActorGeneration: a.actorGeneration,
		Revision:        a.desiredRevision,
		NormalRoutable:  normalRoutable,
		ShadowRoutable:  shadowRoutable,
		Primary:         primaryStatus,
		Candidate:       candidateStatus,
	}
	if sameRouterStatus(a.liveStatus, next) && a.statusEpoch != 0 {
		return
	}

	a.statusEpoch++
	a.liveStatus = next
	if !a.activated {
		return
	}
	_ = a.Send(a.Parent(), MessageRouterStatusChanged{
		pluginID:   a.pluginID,
		pid:        a.PID(),
		generation: a.actorGeneration,
		epoch:      a.statusEpoch,
		status:     next.clone(),
	})
}

func sameRouterStatus(left, right RouterStatus) bool {
	return left.Lifecycle == right.Lifecycle &&
		left.Availability == right.Availability &&
		left.ActorGeneration == right.ActorGeneration &&
		left.Revision == right.Revision &&
		left.NormalRoutable == right.NormalRoutable &&
		left.ShadowRoutable == right.ShadowRoutable &&
		sameDeploymentPoolStatus(left.Primary, right.Primary) &&
		sameDeploymentPoolStatus(left.Candidate, right.Candidate)
}

func (a *routerActor[T]) reportDrained() {
	if a.drainReported {
		return
	}
	a.drainReported = true
	a.reconcileStatus()
	_ = a.Send(a.Parent(), MessageRouterDrained{
		pluginID:   a.pluginID,
		pid:        a.PID(),
		generation: a.actorGeneration,
	})
}

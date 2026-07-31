package plugin

import (
	"context"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
)

// DeploymentPoolLifecycle describes one deployment-pool actor incarnation.
type DeploymentPoolLifecycle string

const (
	DeploymentPoolStarting   DeploymentPoolLifecycle = "starting"
	DeploymentPoolRunning    DeploymentPoolLifecycle = "running"
	DeploymentPoolRestarting DeploymentPoolLifecycle = "restarting"
	DeploymentPoolDraining   DeploymentPoolLifecycle = "draining"
	DeploymentPoolStopped    DeploymentPoolLifecycle = "stopped"
)

// DeploymentPoolStatus is owned by deploymentPoolActor, except for
// RestartCount and ActorLastError, which are owned by routerActor because the
// router owns deployment-pool actor replacement.
type DeploymentPoolStatus struct {
	Lifecycle       DeploymentPoolLifecycle
	Availability    runtime.Availability
	ActorGeneration uint64
	RestartCount    uint64
	RestartPending  bool
	ActorLastError  string

	HealthyWorkers int
	DesiredWorkers int
	QueueDepth     int
	ActiveCalls    int
	Workers        map[int]PluginWorkerStatus
}

func (s DeploymentPoolStatus) clone() DeploymentPoolStatus {
	clone := s
	clone.Workers = make(map[int]PluginWorkerStatus, len(s.Workers))
	for slot, worker := range s.Workers {
		clone.Workers[slot] = worker
	}
	return clone
}

func (s DeploymentPoolStatus) routable() bool {
	return s.Lifecycle == DeploymentPoolRunning && s.Availability.Routable()
}

type workerState[T plugin.Syncable] struct {
	alias            gen.Alias
	workerGeneration uint64
	invocationID     uint64
	status           PluginWorkerStatus
	activeCall       *invokeCall[T]
	restart          *scheduledBackoff
}

type deploymentPoolActor[T plugin.Syncable] struct {
	act.Actor

	deps       actorDependencies[T]
	deployment deployment

	actorGeneration uint64
	activated       bool
	everRoutable    bool

	workers      map[int]workerState[T]
	pendingCalls []invokeCall[T]

	liveStatus  DeploymentPoolStatus
	statusEpoch uint64

	draining      bool
	drainExpired  bool
	drainReported bool
}

type deploymentPoolActivate struct{ generation uint64 }

type deploymentPoolStatusChanged struct {
	poolKey         deploymentPoolKey
	pid             gen.PID
	actorGeneration uint64
	epoch           uint64
	status          DeploymentPoolStatus
}

type deploymentPoolDrained struct {
	poolKey         deploymentPoolKey
	pid             gen.PID
	actorGeneration uint64
}

type pluginWorkerRestart struct {
	slot  int
	token uint64
}

type deploymentPoolHealthTick struct{}
type deploymentPoolDrainDeadline struct{}

func (a *deploymentPoolActor[T]) Init(...any) error {
	if a.workers == nil {
		a.workers = make(map[int]workerState[T])
	}
	return nil
}

func (a *deploymentPoolActor[T]) Terminate(error) {
	a.cancelAllPluginWorkerRestarts(false)
	for _, call := range a.pendingCalls {
		if call.cancel != nil {
			call.cancel()
		}
	}
	for slot := range a.workers {
		a.stopPluginWorker(slot, gen.TerminateReasonShutdown)
	}
}

func (a *deploymentPoolActor[T]) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case deploymentPoolActivate:
		if m.generation <= a.actorGeneration {
			return nil
		}
		if a.activated {
			return fmt.Errorf("deployment pool already activated as generation %d", a.actorGeneration)
		}
		a.actorGeneration = m.generation
		a.activated = true
		for slot := 0; slot < a.deployment.workerCount(); slot++ {
			a.startPluginWorker(slot)
		}
		a.scheduleHealthTick()
		a.reconcileStatus()

	case invokeCall[T]:
		if err := m.context.Err(); err != nil {
			a.finishCall(m, err)
			return nil
		}
		if a.draining || !a.liveStatus.routable() {
			a.finishCall(m, ErrPluginUnavailable)
			return nil
		}
		if len(a.pendingCalls) >= a.deps.queueSize {
			a.finishCall(m, ErrQueueFull)
			return nil
		}
		m.context, m.cancel = context.WithCancel(m.context)
		a.pendingCalls = append(a.pendingCalls, m)
		a.dispatchPendingCalls()

	case cancelCall:
		for i := range a.pendingCalls {
			if a.pendingCalls[i].callID != m.callID {
				continue
			}
			call := a.pendingCalls[i]
			a.pendingCalls = append(a.pendingCalls[:i], a.pendingCalls[i+1:]...)
			a.finishCall(call, m.err)
			a.advanceDrain()
			return nil
		}
		for _, worker := range a.workers {
			if worker.activeCall != nil && worker.activeCall.callID == m.callID {
				worker.activeCall.cancel()
				return nil
			}
		}

	case pluginWorkerStarted:
		worker, ok := a.workers[m.slot]
		if !ok || worker.alias != m.alias || worker.workerGeneration != m.workerGeneration {
			_ = a.SendExitMeta(m.alias, gen.TerminateReasonShutdown)
			return nil
		}
		if worker.activeCall != nil {
			return nil
		}
		worker.status.Lifecycle = PluginWorkerRunning
		worker.status.Availability = runtime.AvailabilityReady
		worker.status.Activity = PluginWorkerIdle
		worker.status.Generation = worker.workerGeneration
		worker.status.RestartPending = false
		worker.status.LastError = ""
		a.workers[m.slot] = worker
		a.resetPluginWorkerRestartBackoff(m.slot)
		a.reconcileStatus()
		a.dispatchPendingCalls()
		a.advanceDrain()

	case pluginWorkerLaunchFailed:
		worker, ok := a.workers[m.slot]
		if !ok || worker.alias != m.alias || worker.workerGeneration != m.workerGeneration {
			return nil
		}
		worker.status.Availability = runtime.AvailabilityUnavailable
		worker.status.LastError = errorText(m.err)
		a.workers[m.slot] = worker
		a.reconcileStatus()

	case pluginWorkerPingResult:
		worker, ok := a.workers[m.slot]
		if !ok || worker.alias != m.alias || worker.workerGeneration != m.workerGeneration {
			return nil
		}
		if m.err != nil {
			worker.status.Availability = runtime.AvailabilityUnavailable
			worker.status.LastError = errorText(m.err)
			a.workers[m.slot] = worker
			a.reconcileStatus()
		}

	case pluginWorkerInvocationFinished:
		worker, ok := a.workers[m.slot]
		if !ok || worker.alias != m.alias || worker.workerGeneration != m.workerGeneration || worker.invocationID != m.invocationID {
			return nil
		}
		if worker.activeCall == nil || worker.activeCall.callID != m.callID {
			return nil
		}

		worker.activeCall.cancel()
		worker.activeCall = nil
		worker.status.InvocationID = worker.invocationID
		worker.status.Activity = PluginWorkerIdle
		if m.recycle {
			worker.status.Availability = runtime.AvailabilityUnavailable
			worker.status.LastError = errorText(errWorkerRecycle)
		} else {
			worker.status.Lifecycle = PluginWorkerRunning
			worker.status.Availability = runtime.AvailabilityReady
			worker.status.LastError = ""
			a.resetPluginWorkerRestartBackoff(m.slot)
		}
		a.workers[m.slot] = worker
		a.completeCall(m.callID, m.err)
		a.reconcileStatus()
		a.dispatchPendingCalls()
		a.advanceDrain()

	case pluginWorkerRestart:
		worker, ok := a.workers[m.slot]
		if !ok || worker.restart == nil || !worker.restart.pending || worker.restart.token != m.token {
			return nil
		}

		worker.restart.pending = false
		worker.restart.cancel = nil
		worker.status.RestartPending = false
		a.workers[m.slot] = worker

		if !a.draining && m.slot < a.deployment.workerCount() {
			a.startPluginWorker(m.slot)
		}

	case deploymentPoolHealthTick:
		if !a.draining {
			for _, worker := range a.workers {
				if worker.alias != (gen.Alias{}) &&
					worker.status.Lifecycle == PluginWorkerRunning &&
					worker.status.Availability == runtime.AvailabilityReady &&
					worker.status.Activity == PluginWorkerIdle {
					_ = a.Send(worker.alias, pluginWorkerPing{})
				}
			}
			a.scheduleHealthTick()
		}

	case drain:
		if a.draining {
			return nil
		}
		a.draining = true
		a.cancelAllPluginWorkerRestarts(false)
		for slot, worker := range a.workers {
			if worker.alias == (gen.Alias{}) {
				a.retirePluginWorker(slot, ErrPluginUnavailable)
				continue
			}
			worker.status.Lifecycle = PluginWorkerDraining
			worker.status.RestartPending = false
			a.workers[slot] = worker
		}
		a.reconcileStatus()
		_, _ = a.SendAfter(a.PID(), deploymentPoolDrainDeadline{}, a.deps.drainTimeout)
		a.advanceDrain()

	case deploymentPoolDrainDeadline:
		if !a.draining || (len(a.pendingCalls) == 0 && a.liveWorkerCount() == 0) {
			return nil
		}
		a.drainExpired = true
		a.cancelQueuedCalls(context.DeadlineExceeded)
		a.stopAllPluginWorkers(gen.TerminateReasonKill)
		a.advanceDrain()

	case stop:
		return gen.TerminateReasonNormal

	case gen.MessageDownAlias:
		for slot, worker := range a.workers {
			if worker.alias != m.Alias {
				continue
			}
			a.handlePluginWorkerExit(slot, worker, m.Reason)
			break
		}
	}
	return nil
}

func (a *deploymentPoolActor[T]) startPluginWorker(slot int) {
	if a.draining || slot >= a.deployment.workerCount() {
		return
	}
	if current, ok := a.workers[slot]; ok && current.alias != (gen.Alias{}) {
		return
	}

	worker := a.workers[slot]
	if worker.alias != (gen.Alias{}) {
		return
	}
	worker.workerGeneration++
	generation := worker.workerGeneration
	worker.invocationID = 0
	worker.activeCall = nil
	worker.status.Lifecycle = PluginWorkerStarting
	worker.status.Availability = runtime.AvailabilityUnavailable
	worker.status.Activity = PluginWorkerIdle
	worker.status.Generation = generation
	worker.status.RestartPending = false
	worker.status.InvocationID = 0
	a.workers[slot] = worker
	a.reconcileStatus()

	alias, err := a.SpawnMeta(&pluginWorkerMeta[T]{
		deps:             a.deps,
		deployment:       a.deployment,
		slot:             slot,
		workerGeneration: generation,
	}, gen.MetaOptions{})
	if err != nil {
		worker := a.workers[slot]
		worker.status.RestartCount++
		worker.status.Lifecycle = PluginWorkerRestarting
		worker.status.LastError = fmt.Sprintf("spawn plugin worker: %v", err)
		a.workers[slot] = worker
		_ = a.schedulePluginWorkerRestart(slot)
		a.reconcileStatus()
		return
	}

	worker = a.workers[slot]
	worker.alias = alias
	a.workers[slot] = worker
	if err := a.MonitorAlias(alias); err != nil {
		_ = a.SendExitMeta(alias, gen.TerminateReasonShutdown)
		worker = a.workers[slot]
		worker.alias = gen.Alias{}
		worker.status.RestartCount++
		worker.status.Lifecycle = PluginWorkerRestarting
		worker.status.LastError = fmt.Sprintf("monitor plugin worker: %v", err)
		a.workers[slot] = worker
		_ = a.schedulePluginWorkerRestart(slot)
		a.reconcileStatus()
	}
}

func (a *deploymentPoolActor[T]) stopPluginWorker(slot int, reason error) {
	worker, ok := a.workers[slot]
	if !ok || worker.alias == (gen.Alias{}) {
		return
	}
	alias := worker.alias
	worker.status.Lifecycle = PluginWorkerDraining
	worker.status.RestartPending = false
	a.workers[slot] = worker
	if err := a.Send(alias, pluginWorkerStop{}); err != nil {
		_ = a.SendExitMeta(alias, reason)
	}
}

func (a *deploymentPoolActor[T]) retirePluginWorker(slot int, callErr error) {
	worker, ok := a.workers[slot]
	if !ok {
		return
	}

	if worker.restart != nil {
		worker.restart.cancelScheduled(false)
	}

	if worker.activeCall != nil {
		call := *worker.activeCall
		worker.activeCall = nil
		a.finishCall(call, callErr)
	}

	worker.alias = gen.Alias{}
	worker.status.Lifecycle = PluginWorkerStopped
	worker.status.Availability = runtime.AvailabilityUnavailable
	worker.status.Activity = PluginWorkerIdle
	worker.status.RestartPending = false

	a.workers[slot] = worker
}

func (a *deploymentPoolActor[T]) schedulePluginWorkerRestart(slot int) error {
	if a.draining {
		return nil
	}

	worker, ok := a.workers[slot]
	if !ok {
		return fmt.Errorf("schedule plugin worker restart: slot %d not found", slot)
	}

	if worker.restart == nil {
		worker.restart = newScheduledBackoff(a.deps.retryMin, a.deps.retryMax)
	}

	if worker.restart.pending {
		a.workers[slot] = worker
		return nil
	}

	delay := worker.restart.strategy.NextBackOff()
	if delay == backoff.Stop {
		a.workers[slot] = worker
		return fmt.Errorf("plugin worker restart backoff stopped for slot %d", slot)
	}

	worker.restart.token++
	token := worker.restart.token
	cancel, err := a.SendAfter(a.PID(), pluginWorkerRestart{slot: slot, token: token}, delay)
	if err != nil {
		return fmt.Errorf("schedule plugin worker restart for slot %d: %w", slot, err)
	}
	worker.restart.pending = true
	worker.restart.cancel = cancel

	worker.status.Lifecycle = PluginWorkerRestarting
	worker.status.Availability = runtime.AvailabilityUnavailable
	worker.status.RestartPending = true
	a.workers[slot] = worker
	a.reconcileStatus()
	return nil
}

func (a *deploymentPoolActor[T]) cancelPluginWorkerRestart(slot int, reset bool) {
	worker, ok := a.workers[slot]
	if !ok {
		return
	}

	if worker.restart != nil {
		worker.restart.cancelScheduled(reset)
	}

	worker.status.RestartPending = false
	a.workers[slot] = worker
}

func (a *deploymentPoolActor[T]) resetPluginWorkerRestartBackoff(slot int) {
	a.cancelPluginWorkerRestart(slot, true)
}

func (a *deploymentPoolActor[T]) cancelAllPluginWorkerRestarts(reset bool) {
	for slot := range a.workers {
		a.cancelPluginWorkerRestart(slot, reset)
	}
}

func (a *deploymentPoolActor[T]) scheduleHealthTick() {
	if a.deps.healthInterval > 0 && !a.draining {
		_, _ = a.SendAfter(a.PID(), deploymentPoolHealthTick{}, a.deps.healthInterval)
	}
}

func (a *deploymentPoolActor[T]) dispatchPendingCalls() {
	for len(a.pendingCalls) > 0 {
		call := a.pendingCalls[0]
		if err := call.context.Err(); err != nil {
			a.pendingCalls = a.pendingCalls[1:]
			a.finishCall(call, err)
			continue
		}

		slot, worker, found := a.nextIdlePluginWorker()
		if !found {
			return
		}

		a.pendingCalls = a.pendingCalls[1:]
		worker.invocationID++
		worker.activeCall = &call
		worker.status.Activity = PluginWorkerBusy
		worker.status.InvocationID = worker.invocationID
		a.workers[slot] = worker
		a.reconcileStatus()

		invoke := pluginWorkerInvoke[T]{
			callID:           call.callID,
			context:          call.context,
			fn:               call.fn,
			workerGeneration: worker.workerGeneration,
			invocationID:     worker.invocationID,
		}
		if err := a.Send(worker.alias, invoke); err != nil {
			_ = a.SendExitMeta(worker.alias, err)
			a.handlePluginWorkerExit(slot, worker, err)
		}
	}
}

func (a *deploymentPoolActor[T]) nextIdlePluginWorker() (int, workerState[T], bool) {
	for slot := 0; slot < a.deployment.workerCount(); slot++ {
		worker, ok := a.workers[slot]
		if ok &&
			worker.alias != (gen.Alias{}) &&
			worker.status.Lifecycle == PluginWorkerRunning &&
			worker.status.Availability == runtime.AvailabilityReady &&
			worker.status.Activity == PluginWorkerIdle {
			return slot, worker, true
		}
	}
	return 0, workerState[T]{}, false
}

func (a *deploymentPoolActor[T]) handlePluginWorkerExit(slot int, worker workerState[T], reason error) {
	current, ok := a.workers[slot]
	if !ok ||
		current.alias != worker.alias ||
		current.workerGeneration != worker.workerGeneration {
		return
	}

	callErr := ErrPluginUnavailable
	if a.drainExpired {
		callErr = context.DeadlineExceeded
	} else if current.activeCall != nil {
		if err := current.activeCall.context.Err(); err != nil {
			callErr = err
		}
	}

	current.alias = gen.Alias{}
	current.status.Availability = runtime.AvailabilityUnavailable
	current.status.Activity = PluginWorkerIdle
	current.status.LastError = errorText(reason)
	current.status.RestartCount++

	if a.draining || slot >= a.deployment.workerCount() {
		a.workers[slot] = current
		a.retirePluginWorker(slot, callErr)
	} else {
		// This call belonged to the failed worker incarnation.
		if current.activeCall != nil {
			call := *current.activeCall
			current.activeCall = nil
			a.finishCall(call, callErr)
		}

		current.status.Lifecycle = PluginWorkerRestarting
		a.workers[slot] = current

		if err := a.schedulePluginWorkerRestart(slot); err != nil {
			current = a.workers[slot]
			current.status.LastError = errorText(err)
			a.workers[slot] = current
		}
	}

	a.reconcileStatus()
	a.advanceDrain()
}

func (a *deploymentPoolActor[T]) stopIdlePluginWorkers() {
	changed := false
	for slot, worker := range a.workers {
		if worker.alias == (gen.Alias{}) ||
			worker.status.Activity != PluginWorkerIdle {
			continue
		}
		worker.status.Lifecycle = PluginWorkerDraining
		a.workers[slot] = worker
		changed = true
		a.stopPluginWorker(slot, gen.TerminateReasonShutdown)
	}
	if changed {
		a.reconcileStatus()
	}
}

func (a *deploymentPoolActor[T]) stopAllPluginWorkers(reason error) {
	for slot, worker := range a.workers {
		if worker.alias == (gen.Alias{}) {
			continue
		}

		if worker.activeCall != nil {
			worker.activeCall.cancel()
		}
		worker.status.Lifecycle = PluginWorkerDraining
		a.workers[slot] = worker
		a.stopPluginWorker(slot, reason)
	}
	a.reconcileStatus()
}

func (a *deploymentPoolActor[T]) liveWorkerCount() int {
	count := 0
	for _, worker := range a.workers {
		if worker.alias != (gen.Alias{}) {
			count++
		}
	}
	return count
}

func (a *deploymentPoolActor[T]) advanceDrain() {
	if !a.draining {
		return
	}
	a.dispatchPendingCalls()
	if len(a.pendingCalls) == 0 {
		a.stopIdlePluginWorkers()
	}
	if len(a.pendingCalls) == 0 && a.liveWorkerCount() == 0 {
		a.reportDrained()
	}
}

func (a *deploymentPoolActor[T]) cancelQueuedCalls(err error) {
	for _, call := range a.pendingCalls {
		a.finishCall(call, err)
	}
	a.pendingCalls = nil
}

func (a *deploymentPoolActor[T]) finishCall(call invokeCall[T], err error) {
	if call.cancel != nil {
		call.cancel()
	}
	a.completeCall(call.callID, err)
}

func (a *deploymentPoolActor[T]) completeCall(callID uint64, err error) {
	_ = a.Send(a.Parent(), callCompleted{callID: callID, err: err})
}

func (a *deploymentPoolActor[T]) reconcileStatus() {
	workers := make(map[int]PluginWorkerStatus, len(a.workers))
	healthy := 0
	activeCalls := 0
	for slot, worker := range a.workers {
		status := worker.status
		status.Generation = worker.workerGeneration
		status.InvocationID = worker.invocationID
		if worker.restart != nil {
			status.RestartPending = worker.restart.pending
		}
		workers[slot] = status
		if status.healthy() {
			healthy++
		}
		if worker.activeCall != nil {
			activeCalls++
		}
	}

	desired := a.deployment.workerCount()
	if desired > 0 && healthy > 0 {
		a.everRoutable = true
	}

	lifecycle := DeploymentPoolStarting
	switch {
	case a.drainReported:
		lifecycle = DeploymentPoolStopped
	case a.draining:
		lifecycle = DeploymentPoolDraining
	case a.everRoutable:
		lifecycle = DeploymentPoolRunning
	}

	availability := runtime.AvailabilityUnavailable
	switch {
	case healthy == 0:
		availability = runtime.AvailabilityUnavailable
	case desired > 0 && healthy == desired:
		availability = runtime.AvailabilityReady
	default:
		availability = runtime.AvailabilityDegraded
	}

	next := DeploymentPoolStatus{
		Lifecycle:       lifecycle,
		Availability:    availability,
		ActorGeneration: a.actorGeneration,
		HealthyWorkers:  healthy,
		DesiredWorkers:  desired,
		QueueDepth:      len(a.pendingCalls),
		ActiveCalls:     activeCalls,
		Workers:         workers,
	}
	if sameDeploymentPoolStatus(a.liveStatus, next) && a.statusEpoch != 0 {
		return
	}

	a.statusEpoch++
	a.liveStatus = next
	if !a.activated {
		return
	}
	_ = a.Send(a.Parent(), deploymentPoolStatusChanged{
		poolKey:         a.deployment.poolKey(),
		pid:             a.PID(),
		actorGeneration: a.actorGeneration,
		epoch:           a.statusEpoch,
		status:          next.clone(),
	})
}

func sameDeploymentPoolStatus(left, right DeploymentPoolStatus) bool {
	if left.Lifecycle != right.Lifecycle ||
		left.Availability != right.Availability ||
		left.ActorGeneration != right.ActorGeneration ||
		left.HealthyWorkers != right.HealthyWorkers ||
		left.DesiredWorkers != right.DesiredWorkers ||
		left.QueueDepth != right.QueueDepth ||
		left.ActiveCalls != right.ActiveCalls ||
		len(left.Workers) != len(right.Workers) {
		return false
	}
	for slot, leftWorker := range left.Workers {
		rightWorker, ok := right.Workers[slot]
		if !ok || leftWorker != rightWorker {
			return false
		}
	}
	return true
}

func (a *deploymentPoolActor[T]) reportDrained() {
	if a.drainReported {
		return
	}
	a.drainReported = true
	a.reconcileStatus()
	_ = a.Send(a.Parent(), deploymentPoolDrained{
		poolKey:         a.deployment.poolKey(),
		pid:             a.PID(),
		actorGeneration: a.actorGeneration,
	})
}

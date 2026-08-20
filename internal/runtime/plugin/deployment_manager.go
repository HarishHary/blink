package plugin

import (
	"context"
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

// DeploymentManagerLifecycle describes the lifecycle of one deployment manager.
type DeploymentManagerLifecycle string

const (
	DeploymentManagerStarting DeploymentManagerLifecycle = "starting"
	DeploymentManagerRunning  DeploymentManagerLifecycle = "running"
	DeploymentManagerDraining DeploymentManagerLifecycle = "draining"
	DeploymentManagerFailed   DeploymentManagerLifecycle = "failed"
	DeploymentManagerStopped  DeploymentManagerLifecycle = "stopped"
)

// deploymentManagerStatus is the manager-owned deployment availability snapshot.
type deploymentManagerStatus struct {
	lifecycle    DeploymentManagerLifecycle
	availability runtime.Availability
	currentProcs int
	readyWorkers int
	queueDepth   int
	dispatching  int
	active       int
	lastError    error
	workers      map[gen.PID]deploymentWorkerStatus
}

// ---------------------------------------------------------------------------
// Invocation State
// ---------------------------------------------------------------------------

// deploymentManagerCallPhase tracks one invocation through the manager pipeline.
type deploymentManagerCallPhase uint8

const (
	deploymentManagerPending deploymentManagerCallPhase = iota
	deploymentManagerDispatching
	deploymentManagerActive
)

// deploymentManagerCall owns one accepted invocation until physical completion.
type deploymentManagerCall[T Artifact] struct {
	call          MessageInvokePlugin[T]
	phase         deploymentManagerCallPhase
	worker        gen.PID
	dispatchToken uint64
	dispatchStop  gen.CancelFunc
	completed     bool
}

// ---------------------------------------------------------------------------
// Deployment Manager
// ---------------------------------------------------------------------------

// deploymentManager owns invocation, scaling, and Pool lifecycle for one concrete deployment.
type deploymentManager[T Artifact] struct {
	act.Actor
	adapter        *Adapter[T]
	options        DeploymentManagerOptions
	draining       bool
	drained        bool
	deployment     Deployment
	route          gen.Atom
	pool           deploymentPoolState
	inFlightCalls  map[uint64]*deploymentManagerCall[T]
	pendingCalls   []uint64
	circuitOpen    bool
	reconcileToken uint64
	reconcileStop  gen.CancelFunc
	idleSince      time.Time
	lastScale      time.Time
	lastError      error
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageDeploymentManagerDispatchDeadline expires an invocation awaiting worker acceptance.
type MessageDeploymentManagerDispatchDeadline struct {
	callID uint64
	token  uint64
}

// MessageDeploymentManagerRestart retries Pool creation after backoff.
type MessageDeploymentManagerRestart struct{ token uint64 }

// MessageDeploymentManagerReconcile runs a token-fenced autoscaling pass.
type MessageDeploymentManagerReconcile struct{ token uint64 }

// MessageDeploymentManagerDrainDeadline expires graceful manager drain.
type MessageDeploymentManagerDrainDeadline struct{}

// MessageDeploymentManagerRetry resets an authenticated open circuit.
type MessageDeploymentManagerRetry struct {
	route   gen.Atom
	manager gen.PID
}

// MessageDeploymentManagerStatusChanged publishes the latest manager status to its Router.
type MessageDeploymentManagerStatusChanged struct {
	route   gen.Atom
	manager gen.PID
	status  deploymentManagerStatus
}

// MessageInvocationAccepted acknowledges manager ownership of an invocation.
type MessageInvocationAccepted struct {
	route   gen.Atom
	manager gen.PID
	callID  uint64
}

// MessageDeploymentManagerDrained reports that no invocation or Pool remains.
type MessageDeploymentManagerDrained struct {
	route   gen.Atom
	manager gen.PID
}

// MessageDeploymentManagerTerminated identifies a stopped manager route incarnation.
type MessageDeploymentManagerTerminated struct {
	route   gen.Atom
	manager gen.PID
	reason  error
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init validates configuration and starts the deployment's minimum Pool capacity.
func (m *deploymentManager[T]) Init(...any) error {
	m.options = deploymentManagerOptionsWithDefaults(m.options)
	if m.deployment.MinProcs < 0 || m.deployment.MaxProcs > 100 || m.deployment.MinProcs > m.deployment.WorkerCount() {
		return fmt.Errorf("deployment manager: invalid worker bounds min=%d max=%d", m.deployment.MinProcs, m.deployment.WorkerCount())
	}
	m.inFlightCalls = make(map[uint64]*deploymentManagerCall[T])
	m.pool.status.workers = make(map[gen.PID]deploymentWorkerStatus)
	if m.draining {
		_, _ = m.SendAfter(m.PID(), MessageDeploymentManagerDrainDeadline{}, m.options.DrainTimeout)
		m.reconcile()
		return nil
	}
	if m.deployment.MinProcs > 0 {
		m.startDeploymentPool(m.deployment.MinProcs)
	}
	m.reconcile()
	return nil
}

// Terminate cancels local work and reports manager termination to the Router.
func (m *deploymentManager[T]) Terminate(reason error) {
	if m.pool.restart != nil {
		m.pool.restart.CancelScheduled(false)
	}
	if m.reconcileStop != nil {
		m.reconcileStop()
	}
	for _, entry := range m.inFlightCalls {
		m.cancel(entry)
	}
	_ = m.SendWithPriority(m.Parent(), MessageDeploymentManagerTerminated{
		route: m.route, manager: m.PID(), reason: reason,
	}, gen.MessagePriorityHigh)
}

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

// HandleMessage processes invocations, child facts, timers, scaling, and drain controls.
func (m *deploymentManager[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageInvokePlugin[T]:
		m.acceptInvocation(msg)

	case MessageCancelInvocation:
		entry := m.inFlightCalls[msg.CallID]
		if entry == nil {
			return nil
		}
		err := msg.Err
		if err == nil {
			err = context.Canceled
		}
		m.cancel(entry)
		if entry.phase == deploymentManagerPending {
			m.removeCall(msg.CallID, err)
		} else {
			m.completeInvocation(entry, err)
		}
		m.reconcile()

	case MessageDeploymentPoolStatusChanged:
		if from != m.pool.pid || msg.pool != m.pool.pid || m.pool.pid == (gen.PID{}) {
			return nil
		}
		m.pool.status = msg.status.clone()
		m.reconcile()

	case MessageDeploymentWorkerRestartExhausted:
		if from != m.pool.pid || m.pool.pid == (gen.PID{}) || m.pool.recovering || m.circuitOpen {
			return nil
		}
		m.lastError = msg.err
		m.pool.recovering = true
		if err := m.Node().SendExit(m.pool.pid, fmt.Errorf("worker restart exhausted: %w", msg.err)); err != nil {
			m.openCircuit(fmt.Errorf("terminate exhausted deployment pool: %w", err))
			return nil
		}
		m.publishStatus()

	case MessageInvocationStarted:
		entry := m.inFlightCalls[msg.callID]
		if entry == nil || entry.phase != deploymentManagerDispatching {
			return nil
		}
		if entry.dispatchStop != nil {
			entry.dispatchStop()
			entry.dispatchStop = nil
		}
		entry.phase, entry.worker = deploymentManagerActive, from
		m.reconcile()

	case MessageInvocationFinished:
		entry := m.inFlightCalls[msg.callID]
		if entry == nil || entry.phase != deploymentManagerActive || entry.worker != from {
			return nil
		}
		m.removeCall(msg.callID, msg.err)
		m.reconcile()

	case MessageDeploymentWorkerStopped:
		if from != m.pool.pid || msg.pool != m.pool.pid || m.pool.pid == (gen.PID{}) {
			return nil
		}
		for callID, entry := range m.inFlightCalls {
			if entry.phase == deploymentManagerActive && entry.worker == msg.worker {
				m.cancel(entry)
				m.removeCall(callID, runtime.ErrPluginUnavailable)
			}
		}
		m.reconcile()

	case MessageDeploymentPoolResized:
		if from != m.pool.pid || msg.pool != m.pool.pid || !m.pool.resizePending {
			return nil
		}
		m.pool.resizePending = false
		if msg.err != nil {
			m.lastError = msg.err
		} else {
			m.lastScale = time.Now()
		}
		m.reconcile()

	case gen.MessageDownPID:
		if msg.PID != m.pool.pid {
			return nil
		}
		m.pool.pid, m.pool.resizePending = gen.PID{}, false
		m.pool.status = deploymentPoolStatus{lifecycle: DeploymentPoolStopped, availability: runtime.AvailabilityUnavailable, workers: make(map[gen.PID]deploymentWorkerStatus)}
		m.lastError = msg.Reason
		for callID, entry := range m.inFlightCalls {
			if entry.phase != deploymentManagerPending {
				m.cancel(entry)
				m.removeCall(callID, runtime.ErrPluginUnavailable)
			}
		}
		expected := m.pool.expectedStop
		m.pool.expectedStop = false
		if expected {
			m.pool.recovering = false
			m.lastError = nil
		}
		if !expected && !m.draining {
			// Every unexpected pool incarnation consumes the finite pool budget,
			// including an idle MinProcs=0 pool whose committed calls just failed.
			m.pool.recovering = true
			m.scheduleDeploymentPoolRestart()
		}
		m.reconcile()

	case MessageDeploymentManagerDispatchDeadline:
		entry := m.inFlightCalls[msg.callID]
		if entry == nil || entry.phase != deploymentManagerDispatching || entry.dispatchToken != msg.token {
			return nil
		}
		m.cancel(entry)
		m.removeCall(msg.callID, runtime.ErrPluginUnavailable)
		m.reconcile()

	case MessageDeploymentManagerRestart:
		if m.pool.restart == nil || !m.pool.restart.Pending || msg.token != m.pool.restart.Token {
			return nil
		}
		m.pool.restart.Pending, m.pool.restart.Cancel = false, nil
		if !m.circuitOpen && m.pool.pid == (gen.PID{}) && (m.pool.recovering || m.deployment.MinProcs > 0 || len(m.pendingCalls) > 0) {
			m.startDeploymentPool(max(1, m.deployment.MinProcs))
		}
		m.reconcile()

	case MessageDeploymentManagerReconcile:
		if msg.token != m.reconcileToken {
			return nil
		}
		m.reconcileStop = nil
		m.reconcile()

	case MessageDrain:
		if !m.draining {
			m.draining = true
			if m.pool.restart != nil {
				m.pool.restart.CancelScheduled(false)
			}
			m.pool.recovering = false
			_, _ = m.SendAfter(m.PID(), MessageDeploymentManagerDrainDeadline{}, m.options.DrainTimeout)
		}
		m.reconcile()

	case MessageDeploymentManagerRetry:
		if from != m.Parent() || !m.circuitOpen || msg.route != m.route || msg.manager != m.PID() {
			return nil
		}
		m.circuitOpen, m.pool.recovering, m.lastError = false, false, nil
		if m.pool.restart != nil {
			m.pool.restart.CancelScheduled(true)
		}
		m.reconcile()

	case MessageDeploymentManagerDrainDeadline:
		if !m.draining || m.drained {
			return nil
		}
		for callID, entry := range m.inFlightCalls {
			m.cancel(entry)
			m.completeInvocation(entry, context.DeadlineExceeded)
			delete(m.inFlightCalls, callID)
		}
		m.pendingCalls = nil
		m.reportDrained()

	case MessageStop:
		return gen.TerminateReasonNormal
	}
	return nil
}

// HandleInspect exposes concise operational manager metrics.
func (m *deploymentManager[T]) HandleInspect(_ gen.PID, _ ...string) map[string]string {
	status := m.status()
	return map[string]string{
		"deployment:availability": string(status.availability),
		"deployment:current":      fmt.Sprintf("%d", status.currentProcs),
		"deployment:ready":        fmt.Sprintf("%d", status.readyWorkers),
		"deployment:queue":        fmt.Sprintf("%d", status.queueDepth),
	}
}

// ---------------------------------------------------------------------------
// Invocation Handling
// ---------------------------------------------------------------------------

// acceptInvocation records one invocation or rejects it with an exact completion.
func (m *deploymentManager[T]) acceptInvocation(call MessageInvokePlugin[T]) {
	_ = m.SendWithPriority(m.Parent(), MessageInvocationAccepted{
		route: m.route, manager: m.PID(), callID: call.CallID,
	}, gen.MessagePriorityHigh)
	if _, exists := m.inFlightCalls[call.CallID]; exists {
		return
	}
	if m.draining || m.circuitOpen {
		m.completeInvocation(&deploymentManagerCall[T]{call: call}, runtime.ErrPluginUnavailable)
		return
	}
	if call.Context == nil {
		call.Context = context.Background()
	}
	if err := call.Context.Err(); err != nil {
		m.completeInvocation(&deploymentManagerCall[T]{call: call}, err)
		return
	}
	if len(m.pendingCalls) >= m.options.QueueSize {
		m.completeInvocation(&deploymentManagerCall[T]{call: call}, runtime.ErrQueueFull)
		return
	}
	m.inFlightCalls[call.CallID] = &deploymentManagerCall[T]{call: call, phase: deploymentManagerPending}
	m.pendingCalls = append(m.pendingCalls, call.CallID)
	m.reconcile()
}

// ---------------------------------------------------------------------------
// Reconciliation and Scaling
// ---------------------------------------------------------------------------

// reconcile advances drain, Pool lifecycle, dispatch, scaling, and status publication.
func (m *deploymentManager[T]) reconcile() {
	if m.draining || m.circuitOpen || m.pool.recovering {
		m.publishStatus()
		if m.draining && !m.drained && len(m.inFlightCalls) == 0 {
			m.reportDrained()
		}
		return
	}
	if len(m.pendingCalls) == 0 && m.dispatching() == 0 {
		if m.idleSince.IsZero() {
			m.idleSince = time.Now()
		}
	} else {
		m.idleSince = time.Time{}
	}
	if m.pool.pid == (gen.PID{}) && !m.pool.recovering && (m.deployment.MinProcs > 0 || len(m.pendingCalls) > 0) && (m.pool.restart == nil || !m.pool.restart.Pending) {
		m.startDeploymentPool(max(1, m.deployment.MinProcs))
	}
	m.dispatchInvocation()
	m.scale()
	m.publishStatus()
	if m.draining && !m.drained && len(m.inFlightCalls) == 0 {
		m.reportDrained()
	}
}

// dispatchInvocation forwards queued invocations while ready Pool capacity is available.
func (m *deploymentManager[T]) dispatchInvocation() {
	if m.draining || m.circuitOpen || m.pool.recovering || m.pool.status.lifecycle == DeploymentPoolFailed {
		return
	}
	for len(m.pendingCalls) > 0 && m.pool.pid != (gen.PID{}) && !m.pool.expectedStop && m.active()+m.dispatching() < min(m.pool.status.desiredWorkers, m.ready()) {
		callID := m.pendingCalls[0]
		m.pendingCalls = m.pendingCalls[1:]
		entry := m.inFlightCalls[callID]
		if entry == nil || entry.phase != deploymentManagerPending {
			continue
		}
		if err := entry.call.Context.Err(); err != nil {
			m.removeCall(callID, err)
			continue
		}
		entry.phase = deploymentManagerDispatching
		entry.dispatchToken++
		cancel, err := m.SendAfter(m.PID(), MessageDeploymentManagerDispatchDeadline{callID: callID, token: entry.dispatchToken}, m.options.DispatchTimeout)
		if err != nil {
			m.removeCall(callID, runtime.ErrPluginUnavailable)
			continue
		}
		entry.dispatchStop = cancel
		if err := m.Send(m.pool.pid, entry.call); err != nil {
			m.removeCall(callID, runtime.ErrPluginUnavailable)
			_ = m.Node().SendExit(m.pool.pid, fmt.Errorf("dispatch invocation: %w", err))
		}
	}
}

// scale changes Pool capacity by at most one worker per reconciliation.
func (m *deploymentManager[T]) scale() {
	if m.draining || m.circuitOpen || m.pool.recovering || m.pool.status.lifecycle == DeploymentPoolFailed {
		return
	}
	if m.pool.resizePending || m.pool.pid == (gen.PID{}) || time.Since(m.lastScale) < m.options.ScaleCooldown {
		m.scheduleScaleReconcile()
		return
	}
	if len(m.pendingCalls) > 0 && m.pool.status.desiredWorkers < m.deployment.WorkerCount() && m.ready() >= m.pool.status.desiredWorkers && m.active()+m.dispatching() >= min(m.pool.status.desiredWorkers, m.ready()) {
		m.pool.resizePending = true
		if err := m.SendWithPriority(m.pool.pid, MessageDeploymentPoolAddWorker{}, gen.MessagePriorityHigh); err != nil {
			m.pool.resizePending = false
			m.lastError = err
		}
		return
	}
	if len(m.pendingCalls) == 0 && m.dispatching() == 0 && m.pool.status.desiredWorkers > m.deployment.MinProcs {
		if time.Since(m.idleSince) < m.options.IdleTimeout {
			m.scheduleScaleReconcile()
			return
		}
		if m.pool.status.desiredWorkers == 1 && m.deployment.MinProcs == 0 {
			if m.active() == 0 {
				m.pool.expectedStop = true
				_ = m.Node().SendExit(m.pool.pid, gen.TerminateReasonNormal)
			}
			return
		}
		m.pool.resizePending = true
		if err := m.SendWithPriority(m.pool.pid, MessageDeploymentPoolRemoveWorker{}, gen.MessagePriorityHigh); err != nil {
			m.pool.resizePending = false
			m.lastError = err
		}
	}
}

// ---------------------------------------------------------------------------
// Pool Lifecycle
// ---------------------------------------------------------------------------

// startDeploymentPool spawns and monitors a fresh Pool incarnation.
func (m *deploymentManager[T]) startDeploymentPool(initial int) {
	if m.draining || m.circuitOpen || m.pool.pid != (gen.PID{}) {
		return
	}
	// LinkParent only propagates manager termination; MonitorPID below receives pool DOWN.
	poolOptions := m.options.PoolOptions
	poolOptions.InitialSize = int64(initial)
	poolOptions.MaxSize = int64(m.deployment.WorkerCount())
	pid, err := m.Spawn(func() gen.ProcessBehavior {
		return &deploymentPool[T]{
			adapter:    m.adapter,
			options:    poolOptions,
			deployment: m.deployment,
		}
	}, gen.ProcessOptions{LinkParent: true})
	if err != nil {
		m.lastError = fmt.Errorf("spawn deployment pool: %w", err)
		m.scheduleDeploymentPoolRestart()
		return
	}
	m.pool.pid = pid
	m.pool.expectedStop = false
	m.pool.recovering = false
	m.pool.status = deploymentPoolStatus{
		lifecycle:      DeploymentPoolStarting,
		availability:   runtime.AvailabilityUnavailable,
		desiredWorkers: initial,
		workers:        make(map[gen.PID]deploymentWorkerStatus),
	}
	if err := m.MonitorPID(pid); err != nil {
		m.lastError = fmt.Errorf("monitor deployment pool: %w", err)
		_ = m.Node().SendExit(pid, gen.TerminateReasonShutdown)
		m.pool.pid = gen.PID{}
		m.pool.status = deploymentPoolStatus{
			lifecycle:    DeploymentPoolStopped,
			availability: runtime.AvailabilityUnavailable,
			workers:      make(map[gen.PID]deploymentWorkerStatus),
		}
		m.scheduleDeploymentPoolRestart()
		return
	}
}

// scheduleDeploymentPoolRestart consumes the finite manager-level retry budget.
func (m *deploymentManager[T]) scheduleDeploymentPoolRestart() {
	m.pool.recovering = true
	if m.pool.restart == nil {
		m.pool.restart = runtime.NewScheduledBackoff(m.options.PoolOptions.RetryMin, m.options.PoolOptions.RetryMax)
	}
	if m.pool.restart.Pending {
		return
	}
	delay := m.pool.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		m.openCircuit(fmt.Errorf("deployment pool restart budget: %w", runtime.ErrBackoffStopped))
		return
	}
	m.pool.restart.Token++
	cancel, err := m.SendAfter(m.PID(), MessageDeploymentManagerRestart{token: m.pool.restart.Token}, delay)
	if err != nil {
		m.openCircuit(fmt.Errorf("schedule deployment pool restart: %w", err))
		return
	}
	m.pool.restart.Pending, m.pool.restart.Cancel = true, cancel
}

// openCircuit stops Pool recovery and fails all tracked invocations.
func (m *deploymentManager[T]) openCircuit(err error) {
	if m.circuitOpen {
		return
	}
	m.circuitOpen, m.pool.recovering, m.lastError = true, false, err
	if m.pool.restart != nil {
		m.pool.restart.CancelScheduled(false)
	}
	for callID := range m.inFlightCalls {
		m.removeCall(callID, runtime.ErrPluginUnavailable)
	}
	m.pendingCalls = nil
	m.publishStatus()
}

// scheduleScaleReconcile replaces the pending autoscaling timer with a fenced one.
func (m *deploymentManager[T]) scheduleScaleReconcile() {
	if m.circuitOpen || m.pool.recovering || m.pool.pid == (gen.PID{}) || m.pool.resizePending {
		return
	}
	delay := time.Duration(0)
	if remaining := m.options.ScaleCooldown - time.Since(m.lastScale); remaining > delay {
		delay = remaining
	}
	if len(m.pendingCalls) == 0 && m.dispatching() == 0 && m.pool.status.desiredWorkers > m.deployment.MinProcs {
		if remaining := m.options.IdleTimeout - time.Since(m.idleSince); remaining > delay {
			delay = remaining
		}
	}
	if delay <= 0 {
		return
	}
	if m.reconcileStop != nil {
		m.reconcileStop()
	}
	m.reconcileToken++
	cancel, err := m.SendAfter(m.PID(), MessageDeploymentManagerReconcile{token: m.reconcileToken}, delay)
	if err == nil {
		m.reconcileStop = cancel
	}
}

// ---------------------------------------------------------------------------
// Invocation Bookkeeping
// ---------------------------------------------------------------------------

// cancel signals the invocation context when a cancellation callback exists.
func (m *deploymentManager[T]) cancel(entry *deploymentManagerCall[T]) {
	if entry.call.Cancel != nil {
		entry.call.Cancel()
	}
}

// removeCall releases all state for one invocation and completes it once.
func (m *deploymentManager[T]) removeCall(callID uint64, err error) {
	entry := m.inFlightCalls[callID]
	if entry == nil {
		return
	}
	if entry.dispatchStop != nil {
		entry.dispatchStop()
		entry.dispatchStop = nil
	}
	for i, pending := range m.pendingCalls {
		if pending == callID {
			m.pendingCalls = append(m.pendingCalls[:i], m.pendingCalls[i+1:]...)
			break
		}
	}
	delete(m.inFlightCalls, callID)
	m.completeInvocation(entry, err)
}

// completeInvocation publishes one idempotent invocation result to the Router.
func (m *deploymentManager[T]) completeInvocation(entry *deploymentManagerCall[T], err error) {
	if entry.completed {
		return
	}
	entry.completed = true
	_ = m.SendWithPriority(m.Parent(), MessageInvocationCompleted{
		CallID: entry.call.CallID,
		Err:    err, Route: m.route, Manager: m.PID(),
	}, gen.MessagePriorityHigh)
}

// reportDrained publishes the manager's terminal graceful-drain fact once.
func (m *deploymentManager[T]) reportDrained() {
	if m.drained {
		return
	}
	m.drained = true
	_ = m.SendWithPriority(m.Parent(), MessageDeploymentManagerDrained{
		route: m.route, manager: m.PID(),
	}, gen.MessagePriorityHigh)
	m.publishStatus()
}

// ---------------------------------------------------------------------------
// Status Reporting
// ---------------------------------------------------------------------------

// ready returns the Pool's currently healthy worker count.
func (m *deploymentManager[T]) ready() int {
	return m.pool.status.healthyWorkers
}

// dispatching counts invocations sent to the Pool but not yet started.
func (m *deploymentManager[T]) dispatching() int {
	count := 0
	for _, entry := range m.inFlightCalls {
		if entry.phase == deploymentManagerDispatching {
			count++
		}
	}
	return count
}

// active counts invocations currently executing in workers.
func (m *deploymentManager[T]) active() int {
	count := 0
	for _, entry := range m.inFlightCalls {
		if entry.phase == deploymentManagerActive {
			count++
		}
	}
	return count
}

// status derives the manager's public snapshot from owned state.
func (m *deploymentManager[T]) status() deploymentManagerStatus {
	zeroPoolIdle := m.pool.pid == (gen.PID{}) && m.deployment.MinProcs == 0 && len(m.inFlightCalls) == 0 &&
		(m.pool.restart == nil || !m.pool.restart.Pending) && m.lastError == nil
	noCommittedCapacity := m.pool.recovering || m.pool.status.lifecycle == DeploymentPoolFailed ||
		(m.ready() == 0 && (m.pool.status.lifecycle == DeploymentPoolRestarting ||
			m.pool.status.lifecycle == DeploymentPoolStarting))
	availability := runtime.AvailabilityUnavailable
	if m.circuitOpen {
		availability = runtime.AvailabilityUnavailable
	} else if m.draining {
		availability = runtime.AvailabilityDegraded
	} else if noCommittedCapacity {
		availability = runtime.AvailabilityUnavailable
	} else if m.pool.pid == (gen.PID{}) {
		if zeroPoolIdle {
			availability = runtime.AvailabilityReady
		}
	} else if m.ready() >= m.deployment.MinProcs && m.deployment.MinProcs > 0 {
		availability = runtime.AvailabilityReady
	} else if m.ready() > 0 {
		availability = runtime.AvailabilityDegraded
	}
	lifecycle := DeploymentManagerRunning
	if m.circuitOpen {
		lifecycle = DeploymentManagerFailed
	} else if m.draining {
		lifecycle = DeploymentManagerDraining
	} else if noCommittedCapacity {
		lifecycle = DeploymentManagerStarting
	} else if m.pool.pid == (gen.PID{}) && !zeroPoolIdle {
		lifecycle = DeploymentManagerStarting
	}
	status := deploymentManagerStatus{
		lifecycle: lifecycle, availability: availability,
		currentProcs: m.pool.status.desiredWorkers, readyWorkers: m.ready(),
		queueDepth: len(m.pendingCalls), dispatching: m.dispatching(), active: m.active(),
		lastError: m.lastError,
		workers:   m.pool.status.clone().workers,
	}
	return status
}

// publishStatus sends the latest manager snapshot to its Router parent.
func (m *deploymentManager[T]) publishStatus() {
	_ = m.SendWithPriority(m.Parent(), MessageDeploymentManagerStatusChanged{
		route: m.route, manager: m.PID(),
		status: m.status(),
	}, gen.MessagePriorityHigh)
}

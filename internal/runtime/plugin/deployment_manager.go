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

// DeploymentManagerStatus is the manager-owned deployment availability snapshot.
type DeploymentManagerStatus struct {
	Lifecycle    DeploymentManagerLifecycle
	Availability runtime.Availability
	CurrentProcs int
	ReadyWorkers int
	QueueDepth   int
	Dispatching  int
	Active       int
	Error        error
	Workers      map[gen.PID]DeploymentWorkerStatus
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
type deploymentManagerCall[T Syncable] struct {
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

// DeploymentManager owns invocation, scaling, and Pool lifecycle for one concrete deployment.
type DeploymentManager[T Syncable] struct {
	act.Actor
	adapter        *Adapter[T]
	options        DeploymentManagerOptions
	deployment     Deployment
	route          gen.Atom
	pool           deploymentPoolState
	calls          map[uint64]*deploymentManagerCall[T]
	pendingCalls   []uint64
	circuitOpen    bool
	reconcileToken uint64
	reconcileStop  gen.CancelFunc
	draining       bool
	drained        bool
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
	status  DeploymentManagerStatus
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
func (m *DeploymentManager[T]) Init(...any) error {
	m.options = deploymentManagerOptionsWithDefaults(m.options)
	if m.deployment.MinProcs < 0 || m.deployment.MaxProcs > 100 || m.deployment.MinProcs > m.deployment.WorkerCount() {
		return fmt.Errorf("deployment manager: invalid worker bounds min=%d max=%d", m.deployment.MinProcs, m.deployment.WorkerCount())
	}
	m.calls = make(map[uint64]*deploymentManagerCall[T])
	m.pool.status.Workers = make(map[gen.PID]DeploymentWorkerStatus)
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
func (m *DeploymentManager[T]) Terminate(reason error) {
	if m.pool.restart != nil {
		m.pool.restart.CancelScheduled(false)
	}
	if m.reconcileStop != nil {
		m.reconcileStop()
	}
	for _, entry := range m.calls {
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
func (m *DeploymentManager[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageInvokePlugin[T]:
		m.acceptInvocation(msg)

	case MessageCancelInvocation:
		entry := m.calls[msg.CallID]
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
		entry := m.calls[msg.callID]
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
		entry := m.calls[msg.callID]
		if entry == nil || entry.phase != deploymentManagerActive || entry.worker != from {
			return nil
		}
		m.removeCall(msg.callID, msg.err)
		m.reconcile()

	case MessageDeploymentWorkerStopped:
		if from != m.pool.pid || msg.pool != m.pool.pid || m.pool.pid == (gen.PID{}) {
			return nil
		}
		for callID, entry := range m.calls {
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
		m.pool.status = DeploymentPoolStatus{Lifecycle: DeploymentPoolStopped, Availability: runtime.AvailabilityUnavailable, Workers: make(map[gen.PID]DeploymentWorkerStatus)}
		m.lastError = msg.Reason
		for callID, entry := range m.calls {
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
		entry := m.calls[msg.callID]
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
		for callID, entry := range m.calls {
			m.cancel(entry)
			m.completeInvocation(entry, context.DeadlineExceeded)
			delete(m.calls, callID)
		}
		m.pendingCalls = nil
		m.reportDrained()

	case MessageStop:
		return gen.TerminateReasonNormal
	}
	return nil
}

// HandleInspect exposes concise operational manager metrics.
func (m *DeploymentManager[T]) HandleInspect(_ gen.PID, _ ...string) map[string]string {
	status := m.status()
	return map[string]string{
		"deployment:availability": string(status.Availability),
		"deployment:current":      fmt.Sprintf("%d", status.CurrentProcs),
		"deployment:ready":        fmt.Sprintf("%d", status.ReadyWorkers),
		"deployment:queue":        fmt.Sprintf("%d", status.QueueDepth),
	}
}

// ---------------------------------------------------------------------------
// Invocation Handling
// ---------------------------------------------------------------------------

// acceptInvocation records one invocation or rejects it with an exact completion.
func (m *DeploymentManager[T]) acceptInvocation(call MessageInvokePlugin[T]) {
	_ = m.SendWithPriority(m.Parent(), MessageInvocationAccepted{
		route: m.route, manager: m.PID(), callID: call.CallID,
	}, gen.MessagePriorityHigh)
	if _, exists := m.calls[call.CallID]; exists {
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
	m.calls[call.CallID] = &deploymentManagerCall[T]{call: call, phase: deploymentManagerPending}
	m.pendingCalls = append(m.pendingCalls, call.CallID)
	m.reconcile()
}

// ---------------------------------------------------------------------------
// Reconciliation and Scaling
// ---------------------------------------------------------------------------

// reconcile advances drain, Pool lifecycle, dispatch, scaling, and status publication.
func (m *DeploymentManager[T]) reconcile() {
	if m.draining || m.circuitOpen || m.pool.recovering {
		m.publishStatus()
		if m.draining && !m.drained && len(m.calls) == 0 {
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
	if m.draining && !m.drained && len(m.calls) == 0 {
		m.reportDrained()
	}
}

// dispatchInvocation forwards queued invocations while ready Pool capacity is available.
func (m *DeploymentManager[T]) dispatchInvocation() {
	if m.draining || m.circuitOpen || m.pool.recovering || m.pool.status.Lifecycle == DeploymentPoolFailed {
		return
	}
	for len(m.pendingCalls) > 0 && m.pool.pid != (gen.PID{}) && !m.pool.expectedStop && m.active()+m.dispatching() < min(m.pool.status.DesiredWorkers, m.ready()) {
		callID := m.pendingCalls[0]
		m.pendingCalls = m.pendingCalls[1:]
		entry := m.calls[callID]
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
func (m *DeploymentManager[T]) scale() {
	if m.draining || m.circuitOpen || m.pool.recovering || m.pool.status.Lifecycle == DeploymentPoolFailed {
		return
	}
	if m.pool.resizePending || m.pool.pid == (gen.PID{}) || time.Since(m.lastScale) < m.options.ScaleCooldown {
		m.scheduleScaleReconcile()
		return
	}
	if len(m.pendingCalls) > 0 && m.pool.status.DesiredWorkers < m.deployment.WorkerCount() && m.ready() >= m.pool.status.DesiredWorkers && m.active()+m.dispatching() >= min(m.pool.status.DesiredWorkers, m.ready()) {
		m.pool.resizePending = true
		if err := m.SendWithPriority(m.pool.pid, MessageDeploymentPoolAddWorker{}, gen.MessagePriorityHigh); err != nil {
			m.pool.resizePending = false
			m.lastError = err
		}
		return
	}
	if len(m.pendingCalls) == 0 && m.dispatching() == 0 && m.pool.status.DesiredWorkers > m.deployment.MinProcs {
		if time.Since(m.idleSince) < m.options.IdleTimeout {
			m.scheduleScaleReconcile()
			return
		}
		if m.pool.status.DesiredWorkers == 1 && m.deployment.MinProcs == 0 {
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
func (m *DeploymentManager[T]) startDeploymentPool(initial int) {
	if m.draining || m.circuitOpen || m.pool.pid != (gen.PID{}) {
		return
	}
	// LinkParent only propagates manager termination; MonitorPID below receives pool DOWN.
	poolOptions := m.options.PoolOptions
	poolOptions.InitialSize = int64(initial)
	poolOptions.MaxSize = int64(m.deployment.WorkerCount())
	pid, err := m.Spawn(func() gen.ProcessBehavior {
		return &DeploymentPool[T]{
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
	m.pool.status = DeploymentPoolStatus{
		Lifecycle:      DeploymentPoolStarting,
		Availability:   runtime.AvailabilityUnavailable,
		DesiredWorkers: initial,
		Workers:        make(map[gen.PID]DeploymentWorkerStatus),
	}
	if err := m.MonitorPID(pid); err != nil {
		m.lastError = fmt.Errorf("monitor deployment pool: %w", err)
		_ = m.Node().SendExit(pid, gen.TerminateReasonShutdown)
		m.pool.pid = gen.PID{}
		m.pool.status = DeploymentPoolStatus{
			Lifecycle:    DeploymentPoolStopped,
			Availability: runtime.AvailabilityUnavailable,
			Workers:      make(map[gen.PID]DeploymentWorkerStatus),
		}
		m.scheduleDeploymentPoolRestart()
		return
	}
}

// scheduleDeploymentPoolRestart consumes the finite manager-level retry budget.
func (m *DeploymentManager[T]) scheduleDeploymentPoolRestart() {
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
func (m *DeploymentManager[T]) openCircuit(err error) {
	if m.circuitOpen {
		return
	}
	m.circuitOpen, m.pool.recovering, m.lastError = true, false, err
	if m.pool.restart != nil {
		m.pool.restart.CancelScheduled(false)
	}
	for callID := range m.calls {
		m.removeCall(callID, runtime.ErrPluginUnavailable)
	}
	m.pendingCalls = nil
	m.publishStatus()
}

// scheduleScaleReconcile replaces the pending autoscaling timer with a fenced one.
func (m *DeploymentManager[T]) scheduleScaleReconcile() {
	if m.circuitOpen || m.pool.recovering || m.pool.pid == (gen.PID{}) || m.pool.resizePending {
		return
	}
	delay := time.Duration(0)
	if remaining := m.options.ScaleCooldown - time.Since(m.lastScale); remaining > delay {
		delay = remaining
	}
	if len(m.pendingCalls) == 0 && m.dispatching() == 0 && m.pool.status.DesiredWorkers > m.deployment.MinProcs {
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
func (m *DeploymentManager[T]) cancel(entry *deploymentManagerCall[T]) {
	if entry.call.Cancel != nil {
		entry.call.Cancel()
	}
}

// removeCall releases all state for one invocation and completes it once.
func (m *DeploymentManager[T]) removeCall(callID uint64, err error) {
	entry := m.calls[callID]
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
	delete(m.calls, callID)
	m.completeInvocation(entry, err)
}

// completeInvocation publishes one idempotent invocation result to the Router.
func (m *DeploymentManager[T]) completeInvocation(entry *deploymentManagerCall[T], err error) {
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
func (m *DeploymentManager[T]) reportDrained() {
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
func (m *DeploymentManager[T]) ready() int {
	return m.pool.status.HealthyWorkers
}

// dispatching counts invocations sent to the Pool but not yet started.
func (m *DeploymentManager[T]) dispatching() int {
	count := 0
	for _, entry := range m.calls {
		if entry.phase == deploymentManagerDispatching {
			count++
		}
	}
	return count
}

// active counts invocations currently executing in workers.
func (m *DeploymentManager[T]) active() int {
	count := 0
	for _, entry := range m.calls {
		if entry.phase == deploymentManagerActive {
			count++
		}
	}
	return count
}

// status derives the manager's public snapshot from owned state.
func (m *DeploymentManager[T]) status() DeploymentManagerStatus {
	zeroPoolIdle := m.pool.pid == (gen.PID{}) && m.deployment.MinProcs == 0 && len(m.calls) == 0 &&
		(m.pool.restart == nil || !m.pool.restart.Pending) && m.lastError == nil
	noCommittedCapacity := m.pool.recovering || m.pool.status.Lifecycle == DeploymentPoolFailed ||
		(m.ready() == 0 && (m.pool.status.Lifecycle == DeploymentPoolRestarting ||
			m.pool.status.Lifecycle == DeploymentPoolStarting))
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
	status := DeploymentManagerStatus{
		Lifecycle: lifecycle, Availability: availability,
		CurrentProcs: m.pool.status.DesiredWorkers, ReadyWorkers: m.ready(),
		QueueDepth: len(m.pendingCalls), Dispatching: m.dispatching(), Active: m.active(),
		Error:   m.lastError,
		Workers: m.pool.status.clone().Workers,
	}
	return status
}

// publishStatus sends the latest manager snapshot to its Router parent.
func (m *DeploymentManager[T]) publishStatus() {
	_ = m.SendWithPriority(m.Parent(), MessageDeploymentManagerStatusChanged{
		route: m.route, manager: m.PID(),
		status: m.status(),
	}, gen.MessagePriorityHigh)
}

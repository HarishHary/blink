package plugin

import (
	"context"
	"fmt"
	"slices"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// DeploymentManagerActorLifecycle describes the lifecycle of one deployment manager.
type DeploymentManagerActorLifecycle string

const (
	DeploymentManagerActorStarting DeploymentManagerActorLifecycle = "starting"
	DeploymentManagerActorRunning  DeploymentManagerActorLifecycle = "running"
	DeploymentManagerActorDraining DeploymentManagerActorLifecycle = "draining"
	DeploymentManagerActorFailed   DeploymentManagerActorLifecycle = "failed"
	DeploymentManagerActorStopped  DeploymentManagerActorLifecycle = "stopped"
)

// deploymentManagerActorStatus is the manager-owned deployment availability snapshot.
type deploymentManagerActorStatus struct {
	lifecycle         DeploymentManagerActorLifecycle
	availability      runtime.Availability
	currentProcs      int
	readyProcs        int
	callsPerProcess   int
	totalCapacity     int
	queueDepth        int
	dispatching       int
	active            int
	availableCapacity int
	processes         map[gen.PID]pluginProcessActorStatus
	err               error
}

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
	process       gen.PID
	dispatchToken uint64
	dispatchStop  gen.CancelFunc
	completed     bool
	queued        bool                      // linked into the manager's pending queue
	prev, next    *deploymentManagerCall[T] // pending queue links, valid while queued
	accepted      time.Time                 // when the manager took the call, for the invocation histogram
}

// pendingQueue is the manager's FIFO of accepted invocations waiting for capacity, linked through the
// call entries so unlinking one costs the same wherever it sits.
type pendingQueue[T Artifact] struct {
	head, tail *deploymentManagerCall[T]
	length     int
}

// push appends one invocation to the back of the queue.
func (q *pendingQueue[T]) push(entry *deploymentManagerCall[T]) {
	if entry.queued {
		return
	}
	entry.queued, entry.prev, entry.next = true, q.tail, nil
	if q.tail != nil {
		q.tail.next = entry
	} else {
		q.head = entry
	}
	q.tail, q.length = entry, q.length+1
}

// pop unlinks and returns the oldest queued invocation, or nil when none is queued.
func (q *pendingQueue[T]) pop() *deploymentManagerCall[T] {
	entry := q.head
	if entry != nil {
		q.remove(entry)
	}
	return entry
}

// remove unlinks one invocation, leaving an unqueued entry alone so every caller that drops a call
// can remove it without knowing which phase the call reached.
func (q *pendingQueue[T]) remove(entry *deploymentManagerCall[T]) {
	if !entry.queued {
		return
	}
	if entry.prev != nil {
		entry.prev.next = entry.next
	} else {
		q.head = entry.next
	}
	if entry.next != nil {
		entry.next.prev = entry.prev
	} else {
		q.tail = entry.prev
	}
	entry.queued, entry.prev, entry.next = false, nil, nil
	q.length--
}

// pluginProcessActorState is one process slot: a stable identity that outlives the PIDs filling it, what the
// current process reported, and the backoff that owns its next start. A zero pid waits on that backoff,
// assigned is the outstanding invocation count the manager schedules from, retiring stops a process once
// its calls finish, and replace refills the slot, separating a failure from a deliberate shrink.
type pluginProcessActorState struct {
	pid         gen.PID
	restart     *runtime.ScheduledBackoff
	status      pluginProcessActorStatus
	statusEpoch int64
	assigned    int
	retiring    bool
	replace     bool
}

// deploymentManagerActor owns invocation, scaling, and process lifecycle for one concrete deployment.
type deploymentManagerActor[T Artifact] struct {
	act.Actor
	adapter    *Adapter[T]
	options    DeploymentManagerOptions
	draining   bool
	drained    bool
	deployment Deployment
	route      gen.Atom
	// processes are this deployment's slots by id, order records the sequence they were opened in so
	// shrinking retires the newest, and byPID resolves a child's facts back to its slot.
	processes map[int]*pluginProcessActorState
	order     []int
	byPID     map[gen.PID]int
	nextSlot  int
	// desiredProcs is how many processes the manager wants running: min_procs at rest, one more per
	// scale-up, and none for an idle deployment that reserves none.
	desiredProcs    int
	inFlightCalls   map[uint64]*deploymentManagerCall[T]
	pendingCalls    pendingQueue[T]
	circuitOpen     bool
	circuitToken    uint64
	circuitStop     gen.CancelFunc
	reconcileToken  uint64
	reconcileStop   gen.CancelFunc
	idleSince       time.Time
	lastScale       time.Time
	growthProcs     int                          // processes held from the process budget, above this deployment's reservation
	stopped         bool                         // set by Terminate, never cleared: the slots it would be derived from are gone
	err             error                        // the manager's own failure, kept apart from its processes' errors
	lastStatus      deploymentManagerActorStatus // last published projection, the baseline reconcileStatus dedupes against
	lastStatusEpoch int64
	labels          telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageDeploymentManagerDispatchDeadline expires an invocation awaiting process acceptance.
type MessageDeploymentManagerDispatchDeadline struct {
	callID uint64
	token  uint64
}

// MessageDeploymentManagerRestart refills one slot after its own backoff.
type MessageDeploymentManagerRestart struct {
	slot  int
	token uint64
}

// MessageDeploymentManagerReconcile runs a token-fenced autoscaling pass.
type MessageDeploymentManagerReconcile struct{ token uint64 }

// MessageDeploymentManagerDrainDeadline expires graceful manager drain.
type MessageDeploymentManagerDrainDeadline struct{}

// MessageDeploymentManagerCircuitCooldown re-arms the restart budget of an open circuit.
type MessageDeploymentManagerCircuitCooldown struct{ token uint64 }

// MessageDeploymentManagerRetry resets an authenticated open circuit.
type MessageDeploymentManagerRetry struct {
	route   gen.Atom
	manager gen.PID
}

// MessageDeploymentManagerStatusChanged publishes the latest manager status to its Router.
type MessageDeploymentManagerStatusChanged struct {
	route       gen.Atom
	manager     gen.PID
	status      deploymentManagerActorStatus
	statusEpoch int64
}

// MessageInvocationAccepted acknowledges manager ownership of an invocation.
type MessageInvocationAccepted struct {
	route   gen.Atom
	manager gen.PID
	callID  uint64
}

// MessageDeploymentManagerDrained reports that no invocation or process remains.
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
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// Init validates configuration and starts the deployment's minimum process count.
func (m *deploymentManagerActor[T]) Init(...any) error {
	m.options = deploymentManagerOptionsWithDefaults(m.options)
	if m.deployment.MinProcs < 0 || m.deployment.MaxProcs > MaxDeploymentProcs || m.deployment.MinProcs > m.deployment.ProcessCountLimit() {
		return fmt.Errorf("deployment manager: invalid process bounds min=%d max=%d", m.deployment.MinProcs, m.deployment.ProcessCountLimit())
	}
	if m.deployment.MaxConcurrentCallsPerProcess > MaxDeploymentCallsPerProcess {
		return fmt.Errorf("deployment manager: invalid process capacity calls=%d max=%d", m.deployment.MaxConcurrentCallsPerProcess, MaxDeploymentCallsPerProcess)
	}
	m.inFlightCalls = make(map[uint64]*deploymentManagerCall[T])
	m.processes = make(map[int]*pluginProcessActorState)
	m.byPID = make(map[gen.PID]int)
	// A route always gets its reservation whatever the budget holds - desired state is not negotiable, and
	// oversubscription is reported so an operator sees it before scaling makes it worse.
	reservation := max(1, m.deployment.MinProcs)
	if reserved, limit := m.options.ProcessBudget.reserve(reservation), m.options.ProcessBudget.limit(); reserved > limit && reserved-reservation <= limit {
		m.Log().Warning("reserved plugin processes exceed the process budget: reserved=%d budget=%d route=%s", reserved, limit, m.route)
	}
	if m.draining {
		_, _ = m.SendWithPriorityAfter(m.PID(), MessageDeploymentManagerDrainDeadline{}, gen.MessagePriorityHigh, m.options.DrainTimeout)
		m.reconcile()
		return nil
	}
	m.desiredProcs = m.deployment.MinProcs
	m.reconcile()
	return nil
}

// Terminate cancels local work and reports manager termination to the Router.
func (m *deploymentManagerActor[T]) Terminate(reason error) {
	m.stopped = true
	m.err = runtime.FirstError(reason, m.err)
	m.reconcileStatus()
	m.cancelPluginProcessRestarts(false)
	m.cancelCircuitCooldown()
	if m.reconcileStop != nil {
		m.reconcileStop()
	}
	m.releaseGrowthProcs()
	m.options.ProcessBudget.reserve(-max(1, m.deployment.MinProcs))
	for _, entry := range m.inFlightCalls {
		if entry.call.Cancel != nil {
			entry.call.Cancel()
		}
	}
	_ = m.SendWithPriority(m.Parent(), MessageDeploymentManagerTerminated{
		route: m.route, manager: m.PID(), reason: reason,
	}, gen.MessagePriorityHigh)
}

// HandleMessage processes invocations, child facts, timers, scaling, and drain controls.
func (m *deploymentManagerActor[T]) HandleMessage(from gen.PID, message any) error {
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
		if entry.call.Cancel != nil {
			entry.call.Cancel()
		}
		if entry.phase == deploymentManagerPending {
			m.removeCall(msg.CallID, err)
		} else {
			m.completeInvocation(entry, err)
		}
		m.reconcile()

	case MessagePluginProcessStatusChanged:
		_, process := m.slotFor(msg.process)
		if process == nil || from != msg.process || msg.statusEpoch <= process.statusEpoch {
			return nil
		}
		process.statusEpoch = msg.statusEpoch
		wasReady := process.status.availability == runtime.AvailabilityReady
		process.status = msg.status
		// A process that came up is the only evidence recovery works; without this a slot's budget only shrinks.
		if !wasReady && process.status.availability == runtime.AvailabilityReady {
			process.restart.Strategy.Reset()
		}
		m.reconcile()

	case MessagePluginProcessRestartExhausted:
		slot, process := m.slotFor(from)
		if process == nil || m.circuitOpen {
			return nil
		}
		// The process gave up on its own subprocess, so this incarnation is spent and only its slot is
		// refilled: the deployment's other processes keep serving the calls they hold.
		process.status.err = msg.err
		m.retireSlot(slot, true, fmt.Errorf("plugin process restart exhausted: %w", msg.err))
		m.reconcile()

	case MessageInvocationStarted:
		entry := m.inFlightCalls[msg.callID]
		if entry == nil || entry.phase != deploymentManagerDispatching || entry.process != from {
			return nil
		}
		if entry.dispatchStop != nil {
			entry.dispatchStop()
			entry.dispatchStop = nil
		}
		entry.phase = deploymentManagerActive
		m.reconcile()

	case MessageInvocationFinished:
		entry := m.inFlightCalls[msg.callID]
		if entry == nil || entry.phase != deploymentManagerActive || entry.process != from {
			return nil
		}
		m.removeCall(msg.callID, msg.err)
		m.reconcile()

	case MessagePluginProcessStopped:
		if _, process := m.slotFor(msg.process); process == nil || from != msg.process {
			return nil
		}
		// The process is on its way out and its DOWN will follow, so failing its calls here rather than
		// waiting means a caller learns as soon as the process itself knows.
		m.failProcessCalls(msg.process, ErrPluginUnavailable)
		m.reconcile()

	case gen.MessageDownPID:
		slot, process := m.slotFor(msg.PID)
		if process == nil {
			return nil
		}
		m.labels.Count(m, metricProcessTerminations, telemetry.TerminationReason(msg.Reason))
		// The slot outlives the PID that filled it, so it is emptied rather than dropped; only its retry budget
		// carries over, since a slot whose processes keep dying is what that budget counts.
		retiring, replace := process.retiring, process.replace
		delete(m.byPID, msg.PID)
		*process = pluginProcessActorState{
			restart: process.restart,
			status:  pluginProcessActorStatus{lifecycle: PluginProcessActorRestarting, availability: runtime.AvailabilityUnavailable},
		}
		m.failProcessCalls(msg.PID, ErrPluginUnavailable)
		m.Log().Warning("plugin process down: slot=%d route=%s retiring=%v replace=%v reason=%v", slot, m.route, retiring, replace, msg.Reason)
		switch {
		case retiring && !replace:
			m.releaseSlot(slot)
		case !m.draining:
			// Every unexpected process incarnation consumes its own slot's finite budget, including an idle
			// MinProcs=0 one whose committed calls just failed.
			process.status.err = msg.Reason
			m.Log().Info("scheduling plugin process restart: slot=%d route=%s", slot, m.route)
			m.schedulePluginProcessRestart(slot)
		default:
			m.releaseSlot(slot)
		}
		m.reconcile()

	case MessageDeploymentManagerDispatchDeadline:
		entry := m.inFlightCalls[msg.callID]
		if entry == nil || entry.phase != deploymentManagerDispatching || entry.dispatchToken != msg.token {
			return nil
		}
		m.labels.Count(m, metricDispatchTimeouts)
		if entry.call.Cancel != nil {
			entry.call.Cancel()
		}
		m.removeCall(msg.callID, ErrPluginUnavailable)
		m.reconcile()

	case MessageDeploymentManagerRestart:
		process := m.processes[msg.slot]
		if process == nil || !process.restart.Pending || msg.token != process.restart.Token {
			m.Log().Warning("plugin process restart message dropped: slot=%d route=%s nilProcess=%v msgToken=%d", msg.slot, m.route, process == nil, msg.token)
			return nil
		}
		m.Log().Info("plugin process restart firing: slot=%d route=%s token=%d", msg.slot, m.route, msg.token)
		process.restart.Pending, process.restart.Cancel = false, nil
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
			m.cancelPluginProcessRestarts(false)
			m.cancelCircuitCooldown()
			_, _ = m.SendWithPriorityAfter(m.PID(), MessageDeploymentManagerDrainDeadline{}, gen.MessagePriorityHigh, m.options.DrainTimeout)
		}
		m.reconcile()

	case MessageDeploymentManagerCircuitCooldown:
		if !m.circuitOpen || m.draining || msg.token != m.circuitToken {
			return nil
		}
		m.closeCircuit()
		m.reconcile()

	case MessageDeploymentManagerRetry:
		if from != m.Parent() || !m.circuitOpen || msg.route != m.route || msg.manager != m.PID() {
			return nil
		}
		m.closeCircuit()
		m.reconcile()

	case MessageDeploymentManagerDrainDeadline:
		if !m.draining || m.drained {
			return nil
		}
		for callID, entry := range m.inFlightCalls {
			if entry.call.Cancel != nil {
				entry.call.Cancel()
			}
			m.removeCall(callID, context.DeadlineExceeded)
		}
		m.reportDrained()

	case MessageStop:
		return gen.TerminateReasonNormal
	}
	return nil
}

// HandleInspect exposes concise operational manager metrics.
func (m *deploymentManagerActor[T]) HandleInspect(_ gen.PID, _ ...string) map[string]string {
	status := m.status()
	// Processes and calls are reported apart: a saturated deployment may be short of processes or of the
	// capacity each one was given, and only one of those is its own to raise.
	return map[string]string{
		"deployment:err":               runtime.ErrorText(status.err),
		"deployment:lifecycle":         string(status.lifecycle),
		"deployment:availability":      string(status.availability),
		"deployment:current":           fmt.Sprintf("%d", status.currentProcs),
		"deployment:ready":             fmt.Sprintf("%d", status.readyProcs),
		"deployment:calls_per_process": fmt.Sprintf("%d", status.callsPerProcess),
		"deployment:capacity":          fmt.Sprintf("%d/%d", status.totalCapacity, m.deployment.MaxInvocationCapacity()),
		"deployment:available":         fmt.Sprintf("%d", status.availableCapacity),
		"deployment:active":            fmt.Sprintf("%d", status.active),
		"deployment:queue":             fmt.Sprintf("%d", status.queueDepth),
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// acceptInvocation records one invocation or rejects it with an exact completion.
func (m *deploymentManagerActor[T]) acceptInvocation(call MessageInvokePlugin[T]) {
	_ = m.SendWithPriority(m.Parent(), MessageInvocationAccepted{
		route: m.route, manager: m.PID(), callID: call.CallID,
	}, gen.MessagePriorityHigh)
	if _, exists := m.inFlightCalls[call.CallID]; exists {
		return
	}
	if m.draining || m.circuitOpen {
		m.completeInvocation(&deploymentManagerCall[T]{call: call}, ErrPluginUnavailable)
		return
	}
	if call.Context == nil {
		call.Context = context.Background()
	}
	if err := call.Context.Err(); err != nil {
		m.completeInvocation(&deploymentManagerCall[T]{call: call}, err)
		return
	}
	if m.pendingCalls.length >= m.options.QueueSize {
		m.labels.Count(m, metricQueueRejects)
		m.completeInvocation(&deploymentManagerCall[T]{call: call}, ErrQueueFull)
		return
	}
	entry := &deploymentManagerCall[T]{call: call, phase: deploymentManagerPending, accepted: time.Now()}
	m.inFlightCalls[call.CallID] = entry
	m.pendingCalls.push(entry)
	m.reconcile()
}

// dispatchInvocation forwards queued invocations while ready process capacity is available.
func (m *deploymentManagerActor[T]) dispatchInvocation() {
	if m.draining || m.circuitOpen {
		return
	}
	for m.pendingCalls.length > 0 {
		slot, process := m.selectProcess()
		if process == nil {
			return
		}
		pid := process.pid
		entry := m.pendingCalls.pop()
		callID := entry.call.CallID
		if err := entry.call.Context.Err(); err != nil {
			m.removeCall(callID, err)
			continue
		}
		entry.phase, entry.process = deploymentManagerDispatching, pid
		entry.dispatchToken++
		cancel, err := m.SendWithPriorityAfter(m.PID(), MessageDeploymentManagerDispatchDeadline{callID: callID, token: entry.dispatchToken}, gen.MessagePriorityHigh, m.options.DispatchTimeout)
		if err != nil {
			m.removeCall(callID, ErrPluginUnavailable)
			continue
		}
		entry.dispatchStop = cancel
		process.assigned++
		if err := m.Send(pid, entry.call); err != nil {
			m.removeCall(callID, ErrPluginUnavailable)
			m.retireSlot(slot, true, fmt.Errorf("dispatch invocation: %w", err))
		}
	}
}

// selectProcess returns the ready slot holding the fewest invocations, or nil when none has spare
// capacity; least-loaded, since stacking a call behind a busy process is latency already paid for.
func (m *deploymentManagerActor[T]) selectProcess() (int, *pluginProcessActorState) {
	selected, best := 0, (*pluginProcessActorState)(nil)
	for _, slot := range m.order {
		process := m.processes[slot]
		if process == nil || process.retiring || process.assigned >= m.deployment.CapacityPerProcess() ||
			process.status.availability != runtime.AvailabilityReady {
			continue
		}
		if best == nil || process.assigned < best.assigned {
			selected, best = slot, process
		}
	}
	return selected, best
}

// reconcile advances drain, process lifecycle, dispatch, scaling, and status publication.
func (m *deploymentManagerActor[T]) reconcile() {
	if m.draining || m.circuitOpen {
		m.reconcileStatus()
		if m.draining && !m.drained && len(m.inFlightCalls) == 0 {
			m.reportDrained()
		}
		return
	}
	if m.pendingCalls.length == 0 && m.dispatchingCalls() == 0 {
		if m.idleSince.IsZero() {
			m.idleSince = time.Now()
		}
	} else {
		m.idleSince = time.Time{}
	}
	m.reconcileProcesses()
	m.dispatchInvocation()
	m.reconcileScale()
	m.reconcileStatus()
}

// reconcileProcesses moves the deployment's slots toward the desired count and fills the empty ones,
// one pass at a time.
func (m *deploymentManagerActor[T]) reconcileProcesses() {
	if m.draining || m.circuitOpen {
		return
	}
	// A deployment that reserves no process still has to answer the call that just arrived, so the first
	// queued invocation is what wakes one.
	if m.desiredProcs < 1 && m.pendingCalls.length > 0 {
		m.desiredProcs = 1
	}
	running := m.runningProcs()
	for i := len(m.order) - 1; i >= 0 && running > m.desiredProcs; i-- {
		// The newest slot holding nothing, not simply the newest: retiring one that still holds calls would
		// stop offering it work while they run, and its next completion retires it anyway.
		if process := m.processes[m.order[i]]; process != nil && !process.retiring && process.assigned == 0 {
			m.retireSlot(m.order[i], false, nil)
			running--
		}
	}
	for _, slot := range slices.Clone(m.order) {
		process := m.processes[slot]
		if process == nil || process.pid != (gen.PID{}) || process.retiring {
			continue
		}
		// An empty slot waiting on its own backoff is left there: filling it now would skip the delay that
		// budget exists to impose, and only this slot is held back by it.
		if process.restart.Pending {
			continue
		}
		if !m.startPluginProcess(slot) {
			return
		}
	}
	for m.runningProcs() < m.desiredProcs {
		if !m.openSlot() {
			return
		}
	}
}

// reconcileScale moves the desired process count toward the one this deployment's demand needs, at
// most one scaling decision per cooldown.
func (m *deploymentManagerActor[T]) reconcileScale() {
	if m.draining || m.circuitOpen {
		return
	}
	if m.restartingProcs() || time.Since(m.lastScale) < m.options.ScaleCooldown {
		m.scheduleScaleReconcile()
		return
	}
	required := m.requiredProcs()
	if required > m.desiredProcs && m.pendingCalls.length > 0 &&
		m.readyProcs() >= m.desiredProcs && m.activeCalls()+m.dispatchingCalls() >= m.committedCapacity() {
		// Growth starts only once every ready process is at capacity with a call still waiting, and then goes
		// to what the queue needs; denied growth is not an error, the cooldown paces the next attempt.
		grown := false
		for m.desiredProcs < required && m.options.ProcessBudget.acquire() {
			m.growthProcs++
			m.desiredProcs++
			grown = true
		}
		m.lastScale = time.Now()
		if !grown {
			m.scheduleScaleReconcile()
			return
		}
		m.labels.Count(m, metricScaleEvents, "up")
		m.reconcileProcesses()
		return
	}
	if required < m.desiredProcs && m.pendingCalls.length == 0 && m.dispatchingCalls() == 0 {
		if time.Since(m.idleSince) < m.options.IdleTimeout {
			m.scheduleScaleReconcile()
			return
		}
		// A process still running invocations is not surplus whatever the aggregate capacity says, so only one
		// holding nothing is given back; its completion reconciles this manager again.
		if !m.idleProcs() {
			m.scheduleScaleReconcile()
			return
		}
		m.desiredProcs--
		m.lastScale = time.Now()
		m.labels.Count(m, metricScaleEvents, "down")
		m.syncGrowthProcs()
		m.reconcileProcesses()
		// One process per cooldown down, and nothing else reconciles a quiet deployment, so the next
		// step is armed here or the shrink stops halfway.
		m.scheduleScaleReconcile()
	}
}

// scheduleScaleReconcile replaces the pending autoscaling timer with a fenced one.
func (m *deploymentManagerActor[T]) scheduleScaleReconcile() {
	if m.circuitOpen || m.draining || m.restartingProcs() {
		return
	}
	delay := time.Duration(0)
	if remaining := m.options.ScaleCooldown - time.Since(m.lastScale); remaining > delay {
		delay = remaining
	}
	if m.pendingCalls.length == 0 && m.dispatchingCalls() == 0 && m.desiredProcs > m.deployment.MinProcs {
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
	cancel, err := m.SendWithPriorityAfter(m.PID(), MessageDeploymentManagerReconcile{token: m.reconcileToken}, gen.MessagePriorityHigh, delay)
	if err == nil {
		m.reconcileStop = cancel
	}
}

// requiredProcs converts the invocations this deployment owes into the process count that would serve
// them, each carrying its whole declared capacity, floored at the reservation and capped at max_procs.
func (m *deploymentManagerActor[T]) requiredProcs() int {
	capacity := m.deployment.CapacityPerProcess()
	demand := m.activeCalls() + m.dispatchingCalls() + m.pendingCalls.length
	required := (demand + capacity - 1) / capacity
	return min(max(required, m.deployment.MinProcs), m.deployment.ProcessCountLimit())
}

// syncGrowthProcs follows the budget down to the process count this deployment now wants, so a
// process it gave up is available to whichever deployment needs it next.
func (m *deploymentManagerActor[T]) syncGrowthProcs() {
	if held := max(0, m.desiredProcs-max(1, m.deployment.MinProcs)); held < m.growthProcs {
		m.options.ProcessBudget.release(m.growthProcs - held)
		m.growthProcs = held
	}
}

// releaseGrowthProcs hands every budgeted process this manager holds back to the process budget.
func (m *deploymentManagerActor[T]) releaseGrowthProcs() {
	m.options.ProcessBudget.release(m.growthProcs)
	m.growthProcs = 0
}

// failProcessCalls completes every invocation dispatched to one process.
func (m *deploymentManagerActor[T]) failProcessCalls(pid gen.PID, err error) {
	for callID, entry := range m.inFlightCalls {
		if entry.phase != deploymentManagerPending && entry.process == pid {
			if entry.call.Cancel != nil {
				entry.call.Cancel()
			}
			m.removeCall(callID, err)
		}
	}
}

// removeCall releases all state for one invocation and completes it once.
func (m *deploymentManagerActor[T]) removeCall(callID uint64, err error) {
	entry := m.inFlightCalls[callID]
	if entry == nil {
		return
	}
	if entry.dispatchStop != nil {
		entry.dispatchStop()
		entry.dispatchStop = nil
	}
	m.pendingCalls.remove(entry)
	delete(m.inFlightCalls, callID)
	if _, process := m.slotFor(entry.process); process != nil && entry.phase != deploymentManagerPending {
		// The capacity this call held returns to the process that held it, and a retiring one that just
		// finished its last call is free to stop.
		if process.assigned > 0 {
			process.assigned--
		}
		if process.retiring && process.assigned == 0 {
			if err := m.SendWithPriority(entry.process, MessageStop{}, gen.MessagePriorityHigh); err != nil {
				_ = m.Node().SendExit(entry.process, gen.TerminateReasonShutdown)
			}
		}
	}
	m.completeInvocation(entry, err)
}

// completeInvocation publishes one idempotent invocation result to the Router.
func (m *deploymentManagerActor[T]) completeInvocation(entry *deploymentManagerCall[T], err error) {
	if entry.completed {
		return
	}
	entry.completed = true
	// A rejected call was never accepted, so it has no duration to report.
	if elapsed, ok := telemetry.ElapsedSeconds(entry.accepted); ok {
		m.labels.Observe(m, metricInvocationTime, elapsed)
	}
	_ = m.SendWithPriority(m.Parent(), MessageInvocationCompleted{
		CallID: entry.call.CallID,
		Err:    err, Route: m.route, Manager: m.PID(),
	}, gen.MessagePriorityHigh)
}

// reportDrained publishes the manager's terminal graceful-drain fact once.
func (m *deploymentManagerActor[T]) reportDrained() {
	if m.drained {
		return
	}
	m.drained = true
	_ = m.SendWithPriority(m.Parent(), MessageDeploymentManagerDrained{
		route: m.route, manager: m.PID(),
	}, gen.MessagePriorityHigh)
	m.reconcileStatus()
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// slotFor resolves one plugin process to the slot it fills, and nil for a PID this manager does not own.
func (m *deploymentManagerActor[T]) slotFor(pid gen.PID) (int, *pluginProcessActorState) {
	slot, ok := m.byPID[pid]
	if !ok {
		return 0, nil
	}
	return slot, m.processes[slot]
}

// openSlot opens one slot and starts the process to fill it, reporting whether the deployment gained one.
func (m *deploymentManagerActor[T]) openSlot() bool {
	slot := m.nextSlot
	m.nextSlot++
	m.processes[slot] = &pluginProcessActorState{
		restart: runtime.NewScheduledBackoff(m.options.RestartMin, m.options.RestartMax),
		status: pluginProcessActorStatus{
			lifecycle:    PluginProcessActorStarting,
			availability: runtime.AvailabilityUnavailable,
		},
	}
	// Slot ids come from a counter rather than a position: a restart message carries the id it is for, and
	// ids grow, so the newest slot is still the highest after one in the middle goes.
	m.order = append(m.order, slot)
	return m.startPluginProcess(slot)
}

// retireSlot takes one slot's process out of service - reason set when it must stop at once rather than
// after the calls it holds, replace set when the slot is to be refilled.
func (m *deploymentManagerActor[T]) retireSlot(slot int, replace bool, reason error) {
	process := m.processes[slot]
	if process == nil || process.retiring {
		return
	}
	// An empty slot has no process to retire and no DOWN coming for one, so it is decided here: refilled
	// by its pending restart, or dropped outright.
	if process.pid == (gen.PID{}) {
		if !replace {
			m.releaseSlot(slot)
		}
		return
	}
	process.retiring, process.replace = true, replace
	if reason != nil {
		m.Log().Warning("retiring plugin process slot: slot=%d replace=%v reason=%v route=%s", slot, replace, reason, m.route)
		_ = m.Node().SendExit(process.pid, reason)
		return
	}
	// MessageStop lets a process holding nothing finish on its own terms, and a send that fails means it
	// cannot be asked, so the signal it cannot refuse is the fallback.
	if process.assigned == 0 {
		if err := m.SendWithPriority(process.pid, MessageStop{}, gen.MessagePriorityHigh); err != nil {
			m.Log().Warning("retiring plugin process slot via forced exit: slot=%d route=%s sendErr=%v", slot, m.route, err)
			_ = m.Node().SendExit(process.pid, gen.TerminateReasonShutdown)
		}
	}
}

// releaseSlot drops one slot and the retry budget it owned, for a deployment that is not refilling it.
func (m *deploymentManagerActor[T]) releaseSlot(slot int) {
	process := m.processes[slot]
	if process == nil {
		return
	}
	process.restart.CancelScheduled(false)
	if process.pid != (gen.PID{}) {
		delete(m.byPID, process.pid)
	}
	delete(m.processes, slot)
	m.order = slices.DeleteFunc(m.order, func(id int) bool { return id == slot })
}

// startPluginProcess spawns and monitors the process for one empty slot, reporting whether the slot
// was filled.
func (m *deploymentManagerActor[T]) startPluginProcess(slot int) bool {
	process := m.processes[slot]
	if process == nil {
		return false
	}
	// LinkParent only propagates manager termination downward, so a process that dies on its own never
	// takes the manager with it; the monitor below is what reports that death back.
	pid, err := m.Spawn(func() gen.ProcessBehavior {
		return &pluginProcessActor[T]{
			adapter:    m.adapter,
			options:    m.options.PluginProcessOptions,
			deployment: m.deployment,
			labels:     m.labels,
		}
	}, gen.ProcessOptions{LinkParent: true})
	if err != nil {
		process.status.err = fmt.Errorf("spawn plugin process: %w", err)
		m.schedulePluginProcessRestart(slot)
		return false
	}
	if err := m.MonitorPID(pid); err != nil {
		process.status.err = fmt.Errorf("monitor plugin process: %w", err)
		_ = m.Node().SendExit(pid, gen.TerminateReasonShutdown)
		m.schedulePluginProcessRestart(slot)
		return false
	}
	process.pid = pid
	process.statusEpoch = 0
	process.status = pluginProcessActorStatus{
		err:          process.status.err,
		lifecycle:    PluginProcessActorStarting,
		availability: runtime.AvailabilityUnavailable,
	}
	m.byPID[pid] = slot
	m.labels.Count(m, metricProcessStarts)
	return true
}

// schedulePluginProcessRestart consumes one slot's finite retry budget, opening the deployment's
// circuit when that slot has spent it.
func (m *deploymentManagerActor[T]) schedulePluginProcessRestart(slot int) {
	process := m.processes[slot]
	if process == nil || process.restart.Pending {
		m.Log().Warning("plugin process restart already pending or slot missing: slot=%d route=%s nilProcess=%v", slot, m.route, process == nil)
		return
	}
	delay := process.restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		m.Log().Warning("plugin process restart budget exhausted, opening circuit: slot=%d route=%s", slot, m.route)
		m.openCircuit(fmt.Errorf("plugin process restart budget: %w", runtime.ErrBackoffStopped))
		return
	}
	process.restart.Token++
	m.Log().Info("plugin process restart scheduled: slot=%d route=%s delay=%s token=%d", slot, m.route, delay, process.restart.Token)
	cancel, err := m.SendWithPriorityAfter(m.PID(), MessageDeploymentManagerRestart{slot: slot, token: process.restart.Token}, gen.MessagePriorityHigh, delay)
	if err != nil {
		m.Log().Error("plugin process restart schedule failed: slot=%d route=%s err=%v", slot, m.route, err)
		m.openCircuit(fmt.Errorf("schedule plugin process restart: %w", err))
		return
	}
	process.restart.Pending, process.restart.Cancel = true, cancel
	m.labels.Count(m, metricProcessRestarts)
}

// cancelPluginProcessRestarts drops every slot's pending start, resetting the budgets when the
// deployment is being given a clean one.
func (m *deploymentManagerActor[T]) cancelPluginProcessRestarts(reset bool) {
	for _, process := range m.processes {
		process.restart.CancelScheduled(reset)
	}
}

// runningProcs counts the slots the manager still means to keep, an empty one waiting on its restart
// included, so a failed slot is not immediately joined by another opened for the same shortfall.
func (m *deploymentManagerActor[T]) runningProcs() int {
	count := 0
	for _, process := range m.processes {
		if !process.retiring || process.replace {
			count++
		}
	}
	return count
}

// restartingProcs reports whether any slot is waiting on its own backoff, which is what paces the
// next scaling decision.
func (m *deploymentManagerActor[T]) restartingProcs() bool {
	for _, process := range m.processes {
		if process.restart.Pending {
			return true
		}
	}
	return false
}

// readyProcs counts the processes serving invocations right now.
func (m *deploymentManagerActor[T]) readyProcs() int {
	count := 0
	for _, process := range m.processes {
		if !process.retiring && process.status.availability == runtime.AvailabilityReady {
			count++
		}
	}
	return count
}

// idleProcs reports whether the manager holds a process it could give back right now: one it has not
// already retired and that is running no invocation.
func (m *deploymentManagerActor[T]) idleProcs() bool {
	for _, process := range m.processes {
		if !process.retiring && process.assigned == 0 {
			return true
		}
	}
	return false
}

// openCircuit stops process recovery, fails all tracked invocations, and arms the cooldown.
func (m *deploymentManagerActor[T]) openCircuit(err error) {
	if m.circuitOpen {
		return
	}
	m.circuitOpen = true
	m.labels.Count(m, metricCircuitOpens)
	m.cancelPluginProcessRestarts(false)
	for callID := range m.inFlightCalls {
		m.removeCall(callID, ErrPluginUnavailable)
	}
	// The deployment answers nothing while its circuit is open, so its slots are only cost; the cooldown
	// below opens a fresh set if it can run at all.
	m.desiredProcs = m.deployment.MinProcs
	m.releaseGrowthProcs()
	for _, slot := range slices.Clone(m.order) {
		m.retireSlot(slot, false, gen.TerminateReasonShutdown)
	}
	// Nothing else clears the circuit, so the cooldown gives the deployment fresh slots with fresh budgets
	// and a genuinely broken one just re-opens it.
	m.cancelCircuitCooldown()
	if cancel, sendErr := m.SendWithPriorityAfter(m.PID(), MessageDeploymentManagerCircuitCooldown{token: m.circuitToken}, gen.MessagePriorityHigh, m.options.CircuitCooldown); sendErr == nil {
		m.circuitStop = cancel
	}
	m.err = err
	m.reconcileStatus()
}

// closeCircuit reopens admission, resetting the retry budget of any slot that outlived the circuit opening.
func (m *deploymentManagerActor[T]) closeCircuit() {
	m.circuitOpen = false
	m.err = nil
	m.cancelCircuitCooldown()
	m.cancelPluginProcessRestarts(true)
}

// cancelCircuitCooldown drops any pending cooldown timer and fences its message.
func (m *deploymentManagerActor[T]) cancelCircuitCooldown() {
	if m.circuitStop != nil {
		m.circuitStop()
		m.circuitStop = nil
	}
	m.circuitToken++
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// committedCapacity is what this deployment can execute at once, counting only ready processes and each
// for the calls it can serve; the per-process figure is the declared one every process enforces.
func (m *deploymentManagerActor[T]) committedCapacity() int {
	return m.readyProcs() * m.deployment.CapacityPerProcess()
}

// dispatchingCalls counts invocations sent to a process but not yet started.
func (m *deploymentManagerActor[T]) dispatchingCalls() int {
	count := 0
	for _, entry := range m.inFlightCalls {
		if entry.phase == deploymentManagerDispatching {
			count++
		}
	}
	return count
}

// activeCalls counts invocations currently executing in a process.
func (m *deploymentManagerActor[T]) activeCalls() int {
	count := 0
	for _, entry := range m.inFlightCalls {
		if entry.phase == deploymentManagerActive {
			count++
		}
	}
	return count
}

// processStatuses snapshots what each owned process last reported, keyed by PID since that is what an
// operator sees, and skipping a slot standing empty between two of them.
func (m *deploymentManagerActor[T]) processStatuses() map[gen.PID]pluginProcessActorStatus {
	statuses := make(map[gen.PID]pluginProcessActorStatus, len(m.processes))
	for _, process := range m.processes {
		if process.pid != (gen.PID{}) {
			statuses[process.pid] = process.status
		}
	}
	return statuses
}

// status derives the manager's public snapshot from owned state.
func (m *deploymentManagerActor[T]) status() deploymentManagerActorStatus {
	// A deployment that reserves nothing and is doing nothing is not broken, it is asleep: it holds no
	// slot, owes no call, and is waiting on neither a retry nor an error.
	idleAtZero := m.desiredProcs == 0 && len(m.processes) == 0 && len(m.inFlightCalls) == 0 &&
		!m.restartingProcs()
	availability := runtime.AvailabilityUnavailable
	switch {
	case m.circuitOpen:
		availability = runtime.AvailabilityUnavailable
	case m.draining:
		availability = runtime.AvailabilityDegraded
	case m.readyProcs() == 0:
		if idleAtZero {
			availability = runtime.AvailabilityReady
		}
	case m.readyProcs() >= m.deployment.MinProcs && m.deployment.MinProcs > 0:
		availability = runtime.AvailabilityReady
	default:
		availability = runtime.AvailabilityDegraded
	}
	lifecycle := DeploymentManagerActorRunning
	switch {
	case m.circuitOpen:
		lifecycle = DeploymentManagerActorFailed
	case m.draining:
		lifecycle = DeploymentManagerActorDraining
	case m.readyProcs() == 0 && !idleAtZero:
		lifecycle = DeploymentManagerActorStarting
	}
	// The manager's own failure wins, then its processes in the order their slots were opened.
	errors := make([]error, 0, len(m.order)+1)
	errors = append(errors, m.err)
	for _, slot := range m.order {
		if process := m.processes[slot]; process != nil {
			errors = append(errors, process.status.err)
		}
	}
	status := deploymentManagerActorStatus{
		lifecycle:         lifecycle,
		availability:      availability,
		currentProcs:      m.desiredProcs,
		readyProcs:        m.readyProcs(),
		callsPerProcess:   m.deployment.CapacityPerProcess(),
		totalCapacity:     m.committedCapacity(),
		queueDepth:        m.pendingCalls.length,
		dispatching:       m.dispatchingCalls(),
		active:            m.activeCalls(),
		availableCapacity: max(0, m.committedCapacity()-m.activeCalls()-m.dispatchingCalls()),
		err:               runtime.FirstError(errors...),
		processes:         m.processStatuses(),
	}
	// Termination outranks whatever the counters still describe: a stopped manager serves nothing.
	if m.stopped {
		status.lifecycle = DeploymentManagerActorStopped
		status.availability = runtime.AvailabilityUnavailable
		status.availableCapacity = 0
	}
	return status
}

// sameDeploymentManagerStatus reports whether two snapshots describe the same health, for publish
// deduplication, excluding the per-invocation counters (see deploymentManagerActorStatus).
func sameDeploymentManagerStatus(left, right deploymentManagerActorStatus) bool {
	if left.lifecycle != right.lifecycle ||
		left.availability != right.availability ||
		left.currentProcs != right.currentProcs ||
		left.readyProcs != right.readyProcs ||
		left.callsPerProcess != right.callsPerProcess ||
		left.totalCapacity != right.totalCapacity ||
		runtime.ErrorText(left.err) != runtime.ErrorText(right.err) ||
		len(left.processes) != len(right.processes) {
		return false
	}
	for pid, process := range left.processes {
		other, ok := right.processes[pid]
		if !ok || !samePluginProcessStatus(process, other) {
			return false
		}
	}
	return true
}

// reconcileStatus compares before caching and publishing a new manager snapshot.
func (m *deploymentManagerActor[T]) reconcileStatus() {
	next := m.status()
	if sameDeploymentManagerStatus(m.lastStatus, next) {
		return
	}
	m.lastStatusEpoch = runtime.NextStatusEpoch(m.lastStatusEpoch)
	m.lastStatus = next
	m.propagateStatus(next)
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (m *deploymentManagerActor[T]) propagateStatus(next deploymentManagerActorStatus) {
	_ = m.SendWithPriority(m.Parent(), MessageDeploymentManagerStatusChanged{
		statusEpoch: m.lastStatusEpoch,
		route:       m.route,
		manager:     m.PID(),
		status:      next,
	}, gen.MessagePriorityHigh)
}

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

// PluginProcessLifecycle describes a plugin process's lifecycle.
type PluginProcessLifecycle string

const (
	PluginProcessStarting   PluginProcessLifecycle = "starting"
	PluginProcessRunning    PluginProcessLifecycle = "running"
	PluginProcessRestarting PluginProcessLifecycle = "restarting"
	PluginProcessFailed     PluginProcessLifecycle = "failed"
)

// pluginProcessStatus is the immutable process snapshot sent to its manager.
type pluginProcessStatus struct {
	lifecycle    PluginProcessLifecycle
	availability runtime.Availability
	meta         pluginMetaStatus
}

// pluginProcessCall is one invocation this process handed to its meta-process and is still waiting for, recording the incarnation that took it so a later answer from a replaced subprocess reads as stale, and the handles that end it early: the context the plugin sees and the backstop timer.
type pluginProcessCall struct {
	manager    gen.PID
	generation uint64
	context    context.Context
	cancel     context.CancelFunc
	timeout    gen.CancelFunc
}

// pluginProcess owns one plugin process meta-process incarnation.
type pluginProcess[T Artifact] struct {
	act.Actor
	adapter    *Adapter[T]
	options    PluginProcessOptions
	deployment Deployment
	pluginMeta pluginMetaState
	calls      map[uint64]*pluginProcessCall
}

// pluginMetaInvokeSlack pads the backstop timer below so it never fires on a meta-process still within its own cancellation grace: both live on timers the scheduler may run late, and the only wrong answer here is calling a healthy subprocess hung.
const pluginMetaInvokeSlack = time.Second

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessagePluginProcessStatusChanged reports a process status update to its manager.
type MessagePluginProcessStatusChanged struct {
	process gen.PID
	status  pluginProcessStatus
}

// MessagePluginProcessStopped reports process shutdown to its manager.
type MessagePluginProcessStopped struct {
	process gen.PID
}

// MessagePluginProcessRestartExhausted reports a terminal local recovery failure.
type MessagePluginProcessRestartExhausted struct {
	err error
}

// MessageInvocationStarted marks an invocation accepted by a plugin process.
type MessageInvocationStarted struct{ callID uint64 }

// MessageInvocationFinished reports an invocation result to its manager.
type MessageInvocationFinished struct {
	callID uint64
	err    error
}

// MessagePluginMetaInvokeTimeout reports that a meta-process never answered an invocation; the call id is fence enough, since a completed call is no longer tracked and an id is never reused.
type MessagePluginMetaInvokeTimeout struct {
	callID uint64
}

// MessagePluginMetaRestart triggers a scheduled meta-process restart.
type MessagePluginMetaRestart struct {
	token  uint64
	health bool
}

// MessagePluginMetaHealthTick triggers a scheduled health check.
type MessagePluginMetaHealthTick struct {
	alias gen.Alias
	token uint64
}

// MessagePluginMetaHealthTimeout reports an unanswered health check.
type MessagePluginMetaHealthTimeout struct {
	alias gen.Alias
	token uint64
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init configures retry state and starts the process meta-process.
func (p *pluginProcess[T]) Init(...any) error {
	p.options = pluginProcessOptionsWithDefaults(p.options)
	p.calls = make(map[uint64]*pluginProcessCall)
	p.pluginMeta.restart = runtime.NewScheduledBackoff(p.options.RetryMin, p.options.RetryMax)
	p.pluginMeta.healthRestart = runtime.NewScheduledBackoff(p.options.RetryMin, p.options.RetryMax)
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		capacity:     p.deployment.CapacityPerProcess(),
	}
	return p.startPluginMeta()
}

// Terminate cancels scheduled recovery, releases in-flight calls, and notifies the manager of shutdown; cancelling is what tells the subprocess to stop working on calls nobody will collect, and the manager fails those calls itself as soon as it sees this process stop.
func (p *pluginProcess[T]) Terminate(error) {
	p.pluginMeta.restart.CancelScheduled(false)
	p.pluginMeta.healthRestart.CancelScheduled(false)
	for callID, entry := range p.calls {
		p.releaseCall(entry)
		delete(p.calls, callID)
	}
	_ = p.SendWithPriority(p.Parent(), MessagePluginProcessStopped{process: p.PID()}, gen.MessagePriorityHigh)
}

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

// HandleMessage processes process lifecycle, health, and invocation messages.
func (p *pluginProcess[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageInvokePlugin[T]:
		p.invoke(from, msg)

	case pluginMetaInvokeResult:
		entry, ok := p.calls[msg.callID]
		if !ok || entry.generation != msg.generation {
			// Either the call is already accounted for or the incarnation that ran it is gone and its calls were failed with it, so there is nothing left to report.
			return nil
		}
		err := msg.err
		if err == nil && entry.context.Err() != nil {
			// The call finished as its caller went away, so report the caller's own reason rather than a success nobody is waiting for; the meta already decided whether the subprocess survives, and a withdrawn caller is no evidence that it should not.
			err = entry.context.Err()
		}
		if msg.recycle && msg.alias == p.pluginMeta.alias {
			// The manager dispatches from its own copy of this process's availability, one message behind, and completing this call is what frees the slot that makes it dispatch again, so the copy has to be corrected first: the completion below provokes that dispatch, so with one process the next pending call has nowhere else to go and would be rejected the moment it lands.
			p.reportUnavailable(err)
		}
		// Complete before retiring so this call reports the failure that caused the recycle and only its siblings inherit the generic one.
		p.completeInvocation(msg.callID, err)
		if msg.recycle {
			p.retirePluginMeta(msg.alias, err, true)
		}

	case MessagePluginMetaInvokeTimeout:
		entry, ok := p.calls[msg.callID]
		if !ok {
			return nil
		}
		// The meta-process had the caller's whole deadline plus its cancellation grace and still has not answered, which is the one case the policy calls a hung subprocess.
		err := fmt.Errorf("%w: plugin process did not answer within its cancellation grace", context.DeadlineExceeded)
		generation := entry.generation
		p.completeInvocation(msg.callID, err)
		if generation == p.pluginMeta.generation {
			p.retirePluginMeta(p.pluginMeta.alias, err, true)
		}

	case MessagePluginMetaStartResult:
		if msg.alias != p.pluginMeta.alias {
			return nil
		}
		if msg.err != nil {
			p.reportUnavailable(msg.err)
			p.cancelHealthCheck()
			p.pluginMeta.alias = gen.Alias{}
			return p.schedulePluginMetaRestart(false)
		}
		if p.pluginMeta.status.availability == runtime.AvailabilityReady {
			return nil
		}
		p.pluginMeta.status = pluginMetaStatus{
			lifecycle:    PluginMetaRunning,
			availability: runtime.AvailabilityReady,
			activity:     PluginMetaIdle,
			capacity:     p.deployment.CapacityPerProcess(),
		}
		p.scheduleHealthCheck(p.pluginMeta.alias)
		p.publishStatus(PluginProcessRunning)

	case MessagePluginMetaRestart:
		restart := p.pluginMeta.restart
		if msg.health {
			restart = p.pluginMeta.healthRestart
		}
		if !restart.Pending || msg.token != restart.Token {
			return nil
		}
		restart.Pending = false
		restart.Cancel = nil
		return p.startPluginMeta()

	case MessagePluginMetaHealthTick:
		if p.pluginMeta.status.availability != runtime.AvailabilityReady || p.pluginMeta.pingPending || msg.alias != p.pluginMeta.alias || msg.token != p.pluginMeta.healthRestart.Token {
			return nil
		}
		p.pluginMeta.pingPending = true
		_, err := p.SendAfter(p.PID(), MessagePluginMetaHealthTimeout{alias: msg.alias, token: msg.token}, pluginMetaPingTimeout)
		if err != nil {
			p.pluginMeta.pingPending = false
			p.retirePluginMeta(msg.alias, fmt.Errorf("schedule plugin process ping timeout: %w", err), true)
			return nil
		}
		if err := p.Send(msg.alias, MessagePluginMetaPing{}); err != nil {
			p.retirePluginMeta(msg.alias, fmt.Errorf("send plugin process ping: %w", err), true)
		}

	case MessagePluginMetaHealthTimeout:
		if !p.pluginMeta.pingPending || msg.alias != p.pluginMeta.alias || msg.token != p.pluginMeta.healthRestart.Token {
			return nil
		}
		p.pluginMeta.pingPending = false
		p.retirePluginMeta(msg.alias, context.DeadlineExceeded, true)

	case MessagePluginMetaPingResult:
		if !p.pluginMeta.pingPending || msg.alias != p.pluginMeta.alias {
			return nil
		}
		p.pluginMeta.pingPending = false
		if msg.err != nil {
			p.retirePluginMeta(msg.alias, fmt.Errorf("plugin process ping: %w", msg.err), true)
			return nil
		}
		p.pluginMeta.restart.CancelScheduled(true)
		p.pluginMeta.healthRestart.CancelScheduled(true)
		p.scheduleHealthCheck(msg.alias)

	case MessageStop:
		return gen.TerminateReasonNormal

	case gen.MessageDownAlias:
		if msg.Alias != p.pluginMeta.alias {
			return nil
		}
		healthFailure := p.pluginMeta.pingPending
		p.cancelHealthCheck()
		p.reportUnavailable(msg.Reason)
		p.pluginMeta.alias = gen.Alias{}
		p.failGenerationCalls(p.pluginMeta.generation, msg.Reason)
		return p.schedulePluginMetaRestart(healthFailure)
	}
	return nil
}

// HandleCall rejects unsupported synchronous process calls.
func (p *pluginProcess[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("unsupported plugin process call %T", request), nil
}

// ---------------------------------------------------------------------------
// Invocation handling
// ---------------------------------------------------------------------------

// invoke accepts one invocation and hands it to the active meta-process without waiting for it: the answer arrives later as a pluginMetaInvokeResult, so this actor keeps taking messages while the plugin works.
func (p *pluginProcess[T]) invoke(manager gen.PID, call MessageInvokePlugin[T]) {
	_ = p.SendWithPriority(manager, MessageInvocationStarted{callID: call.CallID}, gen.MessagePriorityHigh)
	base := call.Context
	if base == nil {
		base = context.Background()
	}
	if err := base.Err(); err != nil {
		p.rejectInvocation(manager, call.CallID, err)
		return
	}
	if call.Fn == nil {
		p.rejectInvocation(manager, call.CallID, fmt.Errorf("invocation function is required"))
		return
	}
	if p.pluginMeta.status.availability != runtime.AvailabilityReady || p.pluginMeta.alias == (gen.Alias{}) {
		p.rejectInvocation(manager, call.CallID, runtime.ErrPluginUnavailable)
		return
	}
	if _, tracked := p.calls[call.CallID]; tracked {
		p.rejectInvocation(manager, call.CallID, fmt.Errorf("plugin invocation %d is already in flight", call.CallID))
		return
	}
	if len(p.calls) >= p.deployment.CapacityPerProcess() {
		// The manager dispatches within the capacity it published, so reaching this means its view of this process is stale; refusing is cheap and keeps the subprocess's contract intact.
		p.rejectInvocation(manager, call.CallID, runtime.ErrQueueFull)
		return
	}

	alias := p.pluginMeta.alias
	ctx, cancel := context.WithTimeout(base, p.options.InvocationTimeout)
	entry := &pluginProcessCall{
		manager:    manager,
		generation: p.pluginMeta.generation,
		context:    ctx,
		cancel:     cancel,
	}
	timeout, err := p.SendAfter(p.PID(), MessagePluginMetaInvokeTimeout{callID: call.CallID}, p.invokeBackstop(ctx))
	if err != nil {
		cancel()
		p.retirePluginMeta(alias, fmt.Errorf("schedule plugin process invocation timeout: %w", err), false)
		p.rejectInvocation(manager, call.CallID, err)
		return
	}
	entry.timeout = timeout
	if err := p.Send(alias, pluginMetaInvoke[T]{
		callID:     call.CallID,
		generation: entry.generation,
		context:    ctx,
		fn:         call.Fn,
	}); err != nil {
		p.releaseCall(entry)
		p.retirePluginMeta(alias, fmt.Errorf("send plugin process invocation: %w", err), true)
		p.rejectInvocation(manager, call.CallID, err)
		return
	}
	p.calls[call.CallID] = entry
	p.refreshActivity()
}

// invokeBackstop is how long to wait for an answer that never came: the meta-process answers a cancelled or expired call within its cancellation grace, so this waits that long past the caller's own deadline, since firing classifies a hung subprocess and racing the meta-process would retire one that honoured cancellation.
func (p *pluginProcess[T]) invokeBackstop(ctx context.Context) time.Duration {
	remaining := p.options.InvocationTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if until := time.Until(deadline); until < remaining {
			remaining = until
		}
	}
	if remaining < 0 {
		remaining = 0
	}
	return remaining + pluginMetaCancelGrace + pluginMetaInvokeSlack
}

// rejectInvocation reports a call this process never handed to its meta-process.
func (p *pluginProcess[T]) rejectInvocation(manager gen.PID, callID uint64, err error) {
	_ = p.SendWithPriority(manager, MessageInvocationFinished{callID: callID, err: err}, gen.MessagePriorityHigh)
}

// completeInvocation reports one tracked call's outcome and releases what it held.
func (p *pluginProcess[T]) completeInvocation(callID uint64, err error) {
	entry, ok := p.calls[callID]
	if !ok {
		return
	}
	delete(p.calls, callID)
	p.releaseCall(entry)
	_ = p.SendWithPriority(entry.manager, MessageInvocationFinished{callID: callID, err: err}, gen.MessagePriorityHigh)
	p.refreshActivity()
}

// failGenerationCalls completes every call still owed by a meta-process incarnation that is gone, since their answers can never arrive and a recycle one call caused is charged to its siblings, which learn the reason rather than waiting for a deadline.
func (p *pluginProcess[T]) failGenerationCalls(generation uint64, cause error) {
	for callID, entry := range p.calls {
		if entry.generation != generation {
			continue
		}
		err := error(runtime.ErrProcessRecycle)
		if cause != nil {
			err = fmt.Errorf("%w: %v", runtime.ErrProcessRecycle, cause)
		}
		p.completeInvocation(callID, err)
	}
}

// releaseCall stops a call's backstop timer and cancels the context the plugin sees.
func (p *pluginProcess[T]) releaseCall(entry *pluginProcessCall) {
	if entry.timeout != nil {
		entry.timeout()
	}
	entry.cancel()
}

// refreshActivity records how loaded the subprocess is and republishes only when that crosses between idle, busy, and saturated: the count moves with every invocation and the manager schedules from its own assignments, so the three labels are the only changes something above this process can act on and the count rides along with them.
func (p *pluginProcess[T]) refreshActivity() {
	if p.pluginMeta.status.availability != runtime.AvailabilityReady {
		return
	}
	capacity := p.deployment.CapacityPerProcess()
	activity := PluginMetaIdle
	switch {
	case len(p.calls) >= capacity:
		activity = PluginMetaSaturated
	case len(p.calls) > 0:
		activity = PluginMetaBusy
	}
	p.pluginMeta.status.inFlight, p.pluginMeta.status.capacity = len(p.calls), capacity
	if p.pluginMeta.status.activity == activity {
		return
	}
	p.pluginMeta.status.activity = activity
	p.publishStatus(PluginProcessRunning)
}

// ---------------------------------------------------------------------------
// Plugin meta lifecycle
// ---------------------------------------------------------------------------

// startPluginMeta creates and monitors the meta-process that owns the subprocess.
func (p *pluginProcess[T]) startPluginMeta() error {
	if p.pluginMeta.alias != (gen.Alias{}) {
		return nil
	}
	p.cancelHealthCheck()
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		capacity:     p.deployment.CapacityPerProcess(),
	}
	p.publishStatus(PluginProcessStarting)
	alias, err := p.SpawnMeta(&pluginProcessMeta[T]{
		adapter:    p.adapter,
		deployment: p.deployment,
	}, gen.MetaOptions{})
	if err != nil {
		p.reportUnavailable(fmt.Errorf("spawn plugin meta: %w", err))
		return p.schedulePluginMetaRestart(false)
	}
	if err := p.MonitorAlias(alias); err != nil {
		_ = p.SendExitMeta(alias, gen.TerminateReasonShutdown)
		p.reportUnavailable(fmt.Errorf("monitor plugin meta: %w", err))
		return p.schedulePluginMetaRestart(false)
	}
	// A fresh incarnation gets a fresh generation, which is what lets a late answer from the previous one read as stale instead of completing a call this one now owns.
	p.pluginMeta.generation++
	p.pluginMeta.alias = alias
	return nil
}

// ---------------------------------------------------------------------------
// Plugin meta recovery
// ---------------------------------------------------------------------------

// schedulePluginMetaRestart schedules normal or health recovery.
func (p *pluginProcess[T]) schedulePluginMetaRestart(health bool) error {
	if p.pluginMeta.status.lifecycle == PluginMetaFailed {
		return nil
	}
	restart := p.pluginMeta.restart
	if health {
		restart = p.pluginMeta.healthRestart
	}
	if restart.Pending {
		return nil
	}
	delay := restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		p.failPluginMeta(fmt.Errorf("plugin process restart budget: %w", runtime.ErrBackoffStopped))
		return nil
	}
	restart.Token++
	token := restart.Token
	cancel, err := p.SendAfter(p.PID(), MessagePluginMetaRestart{token: token, health: health}, delay)
	if err != nil {
		p.failPluginMeta(fmt.Errorf("schedule plugin process restart: %w", err))
		return nil
	}
	restart.Pending = true
	restart.Cancel = cancel
	return nil
}

// failPluginMeta records terminal recovery failure and notifies the manager.
func (p *pluginProcess[T]) failPluginMeta(err error) {
	if p.pluginMeta.status.lifecycle == PluginMetaFailed {
		return
	}
	if p.pluginMeta.status.lastError != nil {
		err = fmt.Errorf("%w: %v", err, p.pluginMeta.status.lastError)
	}
	p.cancelHealthCheck()
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaFailed,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		capacity:     p.deployment.CapacityPerProcess(),
		lastError:    err,
	}
	p.publishStatus(PluginProcessFailed)
	_ = p.SendWithPriority(p.Parent(), MessagePluginProcessRestartExhausted{err: err}, gen.MessagePriorityHigh)
}

// retirePluginMeta stops the active meta-process and begins recovery.
func (p *pluginProcess[T]) retirePluginMeta(alias gen.Alias, err error, health bool) {
	if alias != p.pluginMeta.alias {
		return
	}
	p.cancelHealthCheck()
	p.reportUnavailable(err)
	p.pluginMeta.alias = gen.Alias{}
	p.failGenerationCalls(p.pluginMeta.generation, err)
	_ = p.SendExitMeta(alias, gen.TerminateReasonShutdown)
	_ = p.schedulePluginMetaRestart(health)
}

// ---------------------------------------------------------------------------
// Health monitoring
// ---------------------------------------------------------------------------

// scheduleHealthCheck queues a health check for the active meta-process.
func (p *pluginProcess[T]) scheduleHealthCheck(alias gen.Alias) {
	if p.pluginMeta.status.availability != runtime.AvailabilityReady || alias != p.pluginMeta.alias {
		return
	}
	delay := p.options.HealthInterval
	p.pluginMeta.healthRestart.Token++
	token := p.pluginMeta.healthRestart.Token
	if _, err := p.SendAfter(p.PID(), MessagePluginMetaHealthTick{alias: alias, token: token}, delay); err != nil {
		p.retirePluginMeta(alias, fmt.Errorf("schedule plugin process health check: %w", err), true)
	}
}

// cancelHealthCheck invalidates pending health checks.
func (p *pluginProcess[T]) cancelHealthCheck() {
	p.pluginMeta.healthRestart.Token++
	p.pluginMeta.pingPending = false
}

// ---------------------------------------------------------------------------
// Status projection
// ---------------------------------------------------------------------------

// reportUnavailable records a recoverable meta-process failure, idempotently: the recycle path reports unavailability before completing the call that caused it and then retires the meta-process, which reports it again, and that second report says nothing the manager has not been told while publishing it would cost a reconcile per recycle.
func (p *pluginProcess[T]) reportUnavailable(err error) {
	status := pluginMetaStatus{
		lifecycle:    PluginMetaRestarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		capacity:     p.deployment.CapacityPerProcess(),
		lastError:    err,
	}
	if samePluginMetaStatus(p.pluginMeta.status, status) {
		return
	}
	p.pluginMeta.status = status
	p.publishStatus(PluginProcessRestarting)
}

// publishStatus sends the current process status to its manager.
func (p *pluginProcess[T]) publishStatus(lifecycle PluginProcessLifecycle) {
	_ = p.SendWithPriority(p.Parent(), MessagePluginProcessStatusChanged{
		process: p.PID(),
		status: pluginProcessStatus{
			lifecycle:    lifecycle,
			availability: p.pluginMeta.status.availability,
			meta:         p.pluginMeta.status,
		},
	}, gen.MessagePriorityHigh)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// samePluginProcessStatus compares process status snapshots for publish deduplication, excluding the sampled load on purpose: the count changes with every invocation, the capacity never changes at all, and the activity label already carries the crossings that matter (see pluginMetaStatus).
func samePluginProcessStatus(left, right pluginProcessStatus) bool {
	return left.lifecycle == right.lifecycle && left.availability == right.availability &&
		samePluginMetaStatus(left.meta, right.meta)
}

// samePluginMetaStatus compares meta-process status snapshots on the same terms and for the same reason: it decides whether a status is worth publishing.
func samePluginMetaStatus(left, right pluginMetaStatus) bool {
	return left.lifecycle == right.lifecycle && left.availability == right.availability &&
		left.activity == right.activity && errorText(left.lastError) == errorText(right.lastError)
}

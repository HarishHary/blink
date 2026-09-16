package plugin

import (
	"context"
	"errors"
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

var (
	ErrPluginUnavailable = errors.New("plugin unavailable")
	ErrQueueFull         = errors.New("plugin queue full")
	ErrProcessRecycle    = errors.New("plugin process recycled after transport failure")
)

// PluginProcessActorLifecycle describes a plugin process's lifecycle.
type PluginProcessActorLifecycle string

const (
	PluginProcessActorStarting   PluginProcessActorLifecycle = "starting"
	PluginProcessActorRunning    PluginProcessActorLifecycle = "running"
	PluginProcessActorRestarting PluginProcessActorLifecycle = "restarting"
	PluginProcessActorFailed     PluginProcessActorLifecycle = "failed"
	PluginProcessActorStopped    PluginProcessActorLifecycle = "stopped"
)

// pluginProcessActorStatus is the immutable process snapshot sent to its manager.
type pluginProcessActorStatus struct {
	lifecycle    PluginProcessActorLifecycle
	availability runtime.Availability
	meta         pluginMetaStatus
	lastError    error
}

// pluginProcessCall is one in-flight invocation, tagged with the incarnation that took it so a
// replaced subprocess's answer reads as stale.
type pluginProcessCall struct {
	manager    gen.PID
	generation uint64
	context    context.Context
	cancel     context.CancelFunc
	timeout    gen.CancelFunc
}

// pluginProcessActor owns one plugin process meta-process incarnation.
type pluginProcessActor[T Artifact] struct {
	act.Actor
	adapter         *Adapter[T]
	options         PluginProcessOptions
	deployment      Deployment
	pluginMeta      pluginMetaState
	lastStatus      pluginProcessActorStatus
	lastStatusEpoch int64
	calls           map[uint64]*pluginProcessCall
}

// pluginMetaInvokeSlack pads the backstop timer so a late-scheduled timer never calls a subprocess
// still inside its cancellation grace hung.
const pluginMetaInvokeSlack = time.Second

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessagePluginProcessStatusChanged reports a process status update to its manager.
type MessagePluginProcessStatusChanged struct {
	process     gen.PID
	status      pluginProcessActorStatus
	statusEpoch int64
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

// MessagePluginMetaInvokeTimeout reports an unanswered invocation; the call id fences it, since ids
// are never reused.
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
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// Init configures retry state and starts the process meta-process.
func (p *pluginProcessActor[T]) Init(...any) error {
	p.options = pluginProcessOptionsWithDefaults(p.options)
	p.calls = make(map[uint64]*pluginProcessCall)
	p.pluginMeta.restart = runtime.NewScheduledBackoff(p.options.RestartMin, p.options.RestartMax)
	p.pluginMeta.healthRestart = runtime.NewScheduledBackoff(p.options.RestartMin, p.options.RestartMax)
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		capacity:     p.deployment.CapacityPerProcess(),
	}
	return p.startPluginMeta()
}

// Terminate cancels recovery, releases in-flight calls so the subprocess stops working on answers
// nobody will collect, and tells the manager, which fails those calls itself.
func (p *pluginProcessActor[T]) Terminate(reason error) {
	next := p.status()
	next.lifecycle = PluginProcessActorStopped
	next.availability = runtime.AvailabilityUnavailable
	next.lastError = runtime.FirstError(reason, next.lastError)
	p.reconcileStatus(next)
	p.pluginMeta.restart.CancelScheduled(false)
	p.pluginMeta.healthRestart.CancelScheduled(false)
	for callID, entry := range p.calls {
		p.releaseCall(entry)
		delete(p.calls, callID)
	}
	_ = p.SendWithPriority(p.Parent(), MessagePluginProcessStopped{process: p.PID()}, gen.MessagePriorityHigh)
}

// HandleMessage processes process lifecycle, health, and invocation messages.
func (p *pluginProcessActor[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageInvokePlugin[T]:
		p.invoke(from, msg)

	case pluginMetaInvokeResult:
		entry, ok := p.calls[msg.callID]
		if !ok || entry.generation != msg.generation {
			// Already accounted for, or its incarnation is gone and its calls failed with it.
			return nil
		}
		err := msg.err
		if err == nil && entry.context.Err() != nil {
			// The caller went away, so report its reason rather than a success nobody awaits.
			err = entry.context.Err()
		}
		if msg.recycle && msg.alias == p.pluginMeta.alias {
			// Correct the manager's copy of availability first: the completion below frees the slot that
			// makes it dispatch again, and with one process the next call has nowhere else to go.
			p.reportUnavailable(err)
		}
		// Complete before retiring so this call reports the failure that caused the recycle and only its
		// siblings inherit the generic one.
		p.completeInvocation(msg.callID, err)
		if msg.recycle {
			p.retirePluginMeta(msg.alias, err, true)
		}

	case MessagePluginMetaInvokeTimeout:
		entry, ok := p.calls[msg.callID]
		if !ok {
			return nil
		}
		// The meta-process had the caller's whole deadline plus its cancellation grace and still has not
		// answered, which is the one case the policy calls a hung subprocess.
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
		p.reconcileStatus(p.status())

	case MessagePluginMetaRestart:
		restart := p.pluginMeta.restart
		if msg.health {
			restart = p.pluginMeta.healthRestart
		}
		if !restart.Pending || msg.token != restart.Token {
			p.Log().Warning("plugin meta restart dropped: health=%v pending=%v msgToken=%d currentToken=%d", msg.health, restart.Pending, msg.token, restart.Token)
			return nil
		}
		p.Log().Info("plugin meta restart firing: health=%v token=%d", msg.health, msg.token)
		restart.Pending = false
		restart.Cancel = nil
		return p.startPluginMeta()

	case MessagePluginMetaHealthTick:
		if p.pluginMeta.status.availability != runtime.AvailabilityReady || p.pluginMeta.pingPending || msg.alias != p.pluginMeta.alias || msg.token != p.pluginMeta.healthRestart.Token {
			return nil
		}
		p.pluginMeta.pingPending = true
		_, err := p.SendWithPriorityAfter(p.PID(), MessagePluginMetaHealthTimeout{alias: msg.alias, token: msg.token}, gen.MessagePriorityHigh, pluginMetaPingTimeout)
		if err != nil {
			p.pluginMeta.pingPending = false
			p.retirePluginMeta(msg.alias, fmt.Errorf("schedule plugin process ping timeout: %w", err), true)
			return nil
		}
		if err := p.SendWithPriority(msg.alias, MessagePluginMetaPing{}, gen.MessagePriorityHigh); err != nil {
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
func (p *pluginProcessActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("unsupported plugin process call %T", request), nil
}

// HandleInspect exposes the meta's lifecycle, both restart tracks, and in-flight call depth.
func (p *pluginProcessActor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"process:last_error":             runtime.ErrorText(p.status().lastError),
		"process:meta_last_error":        runtime.ErrorText(p.pluginMeta.status.lastError),
		"process:meta_lifecycle":         string(p.pluginMeta.status.lifecycle),
		"process:meta_availability":      string(p.pluginMeta.status.availability),
		"process:meta_activity":          string(p.pluginMeta.status.activity),
		"process:generation":             fmt.Sprintf("%d", p.pluginMeta.generation),
		"process:has_subprocess":         fmt.Sprintf("%t", p.pluginMeta.alias != (gen.Alias{})),
		"process:calls":                  fmt.Sprintf("%d/%d", len(p.calls), p.pluginMeta.status.capacity),
		"process:restart_pending":        fmt.Sprintf("%t", p.pluginMeta.restart.Pending),
		"process:health_restart_pending": fmt.Sprintf("%t", p.pluginMeta.healthRestart.Pending),
		"process:ping_pending":           fmt.Sprintf("%t", p.pluginMeta.pingPending),
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// invoke hands one invocation to the active meta-process without waiting; the answer arrives later as
// a pluginMetaInvokeResult.
func (p *pluginProcessActor[T]) invoke(manager gen.PID, call MessageInvokePlugin[T]) {
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
		p.rejectInvocation(manager, call.CallID, ErrPluginUnavailable)
		return
	}
	if _, tracked := p.calls[call.CallID]; tracked {
		p.rejectInvocation(manager, call.CallID, fmt.Errorf("plugin invocation %d is already in flight", call.CallID))
		return
	}
	if len(p.calls) >= p.deployment.CapacityPerProcess() {
		// The manager dispatches within the capacity it published, so its view is stale; refusing is
		// cheap and keeps the subprocess's contract intact.
		p.rejectInvocation(manager, call.CallID, ErrQueueFull)
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
	timeout, err := p.SendWithPriorityAfter(p.PID(), MessagePluginMetaInvokeTimeout{callID: call.CallID}, gen.MessagePriorityHigh, p.invokeBackstop(ctx))
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

// invokeBackstop waits the cancellation grace past the caller's deadline, since firing classifies a
// hung subprocess and racing the meta-process would retire one that honoured cancellation.
func (p *pluginProcessActor[T]) invokeBackstop(ctx context.Context) time.Duration {
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
func (p *pluginProcessActor[T]) rejectInvocation(manager gen.PID, callID uint64, err error) {
	_ = p.SendWithPriority(manager, MessageInvocationFinished{callID: callID, err: err}, gen.MessagePriorityHigh)
}

// completeInvocation reports one tracked call's outcome and releases what it held.
func (p *pluginProcessActor[T]) completeInvocation(callID uint64, err error) {
	entry, ok := p.calls[callID]
	if !ok {
		return
	}
	delete(p.calls, callID)
	p.releaseCall(entry)
	_ = p.SendWithPriority(entry.manager, MessageInvocationFinished{callID: callID, err: err}, gen.MessagePriorityHigh)
	p.refreshActivity()
}

// failGenerationCalls completes every call owed by a gone incarnation, whose answers can never arrive,
// so siblings learn the reason instead of waiting for a deadline.
func (p *pluginProcessActor[T]) failGenerationCalls(generation uint64, cause error) {
	for callID, entry := range p.calls {
		if entry.generation != generation {
			continue
		}
		err := error(ErrProcessRecycle)
		if cause != nil {
			err = fmt.Errorf("%w: %w", ErrProcessRecycle, cause)
		}
		p.completeInvocation(callID, err)
	}
}

// releaseCall stops a call's backstop timer and cancels the context the plugin sees.
func (p *pluginProcessActor[T]) releaseCall(entry *pluginProcessCall) {
	if entry.timeout != nil {
		entry.timeout()
	}
	entry.cancel()
}

// refreshActivity republishes only on a crossing between idle, busy, and saturated, the only load
// changes anything above this process can act on; the in-flight count rides along.
func (p *pluginProcessActor[T]) refreshActivity() {
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
	p.reconcileStatus(p.status())
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// startPluginMeta creates and monitors the meta-process that owns the subprocess.
func (p *pluginProcessActor[T]) startPluginMeta() error {
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
	p.reconcileStatus(p.status())
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
	// A fresh incarnation gets a fresh generation, which is what lets a late answer from the previous
	// one read as stale instead of completing a call this one now owns.
	p.pluginMeta.generation++
	p.pluginMeta.alias = alias
	return nil
}

// schedulePluginMetaRestart schedules normal or health recovery.
func (p *pluginProcessActor[T]) schedulePluginMetaRestart(health bool) error {
	if p.pluginMeta.status.lifecycle == PluginMetaFailed {
		return nil
	}
	restart := p.pluginMeta.restart
	if health {
		restart = p.pluginMeta.healthRestart
	}
	if restart.Pending {
		p.Log().Warning("plugin meta restart already pending: health=%v token=%d", health, restart.Token)
		return nil
	}
	delay := restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		p.Log().Warning("plugin meta restart budget exhausted: health=%v", health)
		p.failPluginMeta(fmt.Errorf("plugin process restart budget: %w", runtime.ErrBackoffStopped))
		return nil
	}
	restart.Token++
	token := restart.Token
	p.Log().Info("scheduling plugin meta restart: health=%v delay=%s token=%d", health, delay, token)
	cancel, err := p.SendWithPriorityAfter(p.PID(), MessagePluginMetaRestart{token: token, health: health}, gen.MessagePriorityHigh, delay)
	if err != nil {
		p.Log().Error("schedule plugin meta restart failed: health=%v err=%v", health, err)
		p.failPluginMeta(fmt.Errorf("schedule plugin process restart: %w", err))
		return nil
	}
	restart.Pending = true
	restart.Cancel = cancel
	return nil
}

// failPluginMeta records terminal recovery failure and notifies the manager.
func (p *pluginProcessActor[T]) failPluginMeta(err error) {
	if p.pluginMeta.status.lifecycle == PluginMetaFailed {
		return
	}
	if p.pluginMeta.status.lastError != nil {
		err = fmt.Errorf("%w: %w", err, p.pluginMeta.status.lastError)
	}
	p.cancelHealthCheck()
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaFailed,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		capacity:     p.deployment.CapacityPerProcess(),
		lastError:    err,
	}
	p.reconcileStatus(p.status())
	_ = p.SendWithPriority(p.Parent(), MessagePluginProcessRestartExhausted{err: err}, gen.MessagePriorityHigh)
}

// retirePluginMeta stops the active meta-process and begins recovery.
func (p *pluginProcessActor[T]) retirePluginMeta(alias gen.Alias, err error, health bool) {
	if alias != p.pluginMeta.alias {
		return
	}
	p.Log().Info("retiring plugin meta: health=%v generation=%d err=%v", health, p.pluginMeta.generation, err)
	p.cancelHealthCheck()
	p.reportUnavailable(err)
	p.pluginMeta.alias = gen.Alias{}
	p.failGenerationCalls(p.pluginMeta.generation, err)
	_ = p.SendExitMeta(alias, gen.TerminateReasonShutdown)
	_ = p.schedulePluginMetaRestart(health)
}

// scheduleHealthCheck queues a health check for the active meta-process.
func (p *pluginProcessActor[T]) scheduleHealthCheck(alias gen.Alias) {
	if p.pluginMeta.status.availability != runtime.AvailabilityReady || alias != p.pluginMeta.alias {
		return
	}
	delay := p.options.HealthInterval
	p.pluginMeta.healthRestart.Token++
	token := p.pluginMeta.healthRestart.Token
	if _, err := p.SendWithPriorityAfter(p.PID(), MessagePluginMetaHealthTick{alias: alias, token: token}, gen.MessagePriorityHigh, delay); err != nil {
		p.retirePluginMeta(alias, fmt.Errorf("schedule plugin process health check: %w", err), true)
	}
}

// cancelHealthCheck invalidates pending health checks.
func (p *pluginProcessActor[T]) cancelHealthCheck() {
	p.pluginMeta.healthRestart.Token++
	p.pluginMeta.pingPending = false
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// reportUnavailable records a recoverable meta-process failure; reconcileStatus deduplicates repeats.
func (p *pluginProcessActor[T]) reportUnavailable(err error) {
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaRestarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		capacity:     p.deployment.CapacityPerProcess(),
		lastError:    err,
	}
	p.reconcileStatus(p.status())
}

// status derives process health from the meta status owned by this actor.
func (p *pluginProcessActor[T]) status() pluginProcessActorStatus {
	if p.lastStatus.lifecycle == PluginProcessActorStopped {
		return p.lastStatus
	}
	lifecycle := PluginProcessActorStarting
	switch p.pluginMeta.status.lifecycle {
	case PluginMetaRunning:
		lifecycle = PluginProcessActorRunning
	case PluginMetaRestarting:
		lifecycle = PluginProcessActorRestarting
	case PluginMetaFailed:
		lifecycle = PluginProcessActorFailed
	}
	return pluginProcessActorStatus{
		lifecycle:    lifecycle,
		availability: p.pluginMeta.status.availability,
		meta:         p.pluginMeta.status,
		lastError:    p.pluginMeta.status.lastError,
	}
}

// reconcileStatus propagates changed health immediately; the runtime supervisor owns the gauges.
// Failure status must precede invocation completion, which can reopen manager admission.
func (p *pluginProcessActor[T]) reconcileStatus(next pluginProcessActorStatus) {
	if samePluginProcessStatus(p.lastStatus, next) {
		return
	}
	p.lastStatusEpoch = runtime.NextStatusEpoch(p.lastStatusEpoch)
	p.lastStatus = next
	p.propagateStatus(next)
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (p *pluginProcessActor[T]) propagateStatus(next pluginProcessActorStatus) {
	_ = p.SendWithPriority(p.Parent(), MessagePluginProcessStatusChanged{
		statusEpoch: p.lastStatusEpoch,
		process:     p.PID(),
		status:      next,
	}, gen.MessagePriorityHigh)
}

// samePluginProcessStatus dedups status publishes, ignoring sampled load: the activity label already
// carries the crossings that matter (see pluginMetaStatus).
func samePluginProcessStatus(left, right pluginProcessActorStatus) bool {
	return left.lifecycle == right.lifecycle &&
		left.availability == right.availability &&
		runtime.ErrorText(left.lastError) == runtime.ErrorText(right.lastError) &&
		samePluginMetaStatus(left.meta, right.meta)
}

// samePluginMetaStatus compares meta-process status snapshots on the same terms and for the same
// reason: it decides whether a status is worth publishing.
func samePluginMetaStatus(left, right pluginMetaStatus) bool {
	return left.lifecycle == right.lifecycle &&
		left.availability == right.availability &&
		left.activity == right.activity &&
		runtime.ErrorText(left.lastError) == runtime.ErrorText(right.lastError)
}

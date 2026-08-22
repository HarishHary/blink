package plugin

import (
	"context"
	"fmt"

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

// pluginProcess owns one plugin process meta-process incarnation.
type pluginProcess[T Artifact] struct {
	act.Actor
	adapter    *Adapter[T]
	options    PluginProcessOptions
	deployment Deployment
	pluginMeta pluginMetaState
}

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
	p.pluginMeta.restart = runtime.NewScheduledBackoff(p.options.RetryMin, p.options.RetryMax)
	p.pluginMeta.healthRestart = runtime.NewScheduledBackoff(p.options.RetryMin, p.options.RetryMax)
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
	}
	return p.startPluginMeta()
}

// Terminate cancels scheduled recovery and notifies the manager of shutdown.
func (p *pluginProcess[T]) Terminate(error) {
	p.pluginMeta.restart.CancelScheduled(false)
	p.pluginMeta.healthRestart.CancelScheduled(false)
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
		return p.schedulePluginMetaRestart(healthFailure)
	}
	return nil
}

// HandleCall rejects unsupported synchronous process calls.
func (p *pluginProcess[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported plugin process call %T", request), nil
}

// ---------------------------------------------------------------------------
// Invocation handling
// ---------------------------------------------------------------------------

// invoke executes one plugin invocation through the active meta-process.
func (p *pluginProcess[T]) invoke(manager gen.PID, call MessageInvokePlugin[T]) {
	ctx := call.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, p.options.InvocationTimeout)
	defer cancel()
	_ = p.SendWithPriority(manager, MessageInvocationStarted{callID: call.CallID}, gen.MessagePriorityHigh)
	if err := ctx.Err(); err != nil {
		_ = p.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: err}, gen.MessagePriorityHigh)
		return
	}
	if call.Fn == nil {
		_ = p.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: fmt.Errorf("actorruntime: invocation function is required")}, gen.MessagePriorityHigh)
		return
	}
	if p.pluginMeta.status.availability != runtime.AvailabilityReady || p.pluginMeta.alias == (gen.Alias{}) {
		_ = p.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: runtime.ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return
	}

	alias := p.pluginMeta.alias
	p.pluginMeta.status.activity = PluginMetaBusy
	p.publishStatus(PluginProcessRunning)
	response, err := p.CallWithTimeout(alias, pluginMetaInvoke[T]{context: ctx, fn: call.Fn}, callTimeoutSeconds(ctx, p.options.InvocationTimeout))
	p.pluginMeta.status.activity = PluginMetaIdle
	p.publishStatus(PluginProcessRunning)
	recycle := err != nil
	if err == nil {
		result, ok := response.(pluginMetaInvokeResult)
		if !ok {
			err = fmt.Errorf("actorruntime: unexpected plugin process response %T", response)
			recycle = true
		} else {
			err = result.err
			recycle = result.recycle
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
		recycle = true
	}
	if recycle {
		p.retirePluginMeta(alias, err, true)
	}
	_ = p.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: err}, gen.MessagePriorityHigh)
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

// reportUnavailable records a recoverable meta-process failure.
func (p *pluginProcess[T]) reportUnavailable(err error) {
	p.pluginMeta.status = pluginMetaStatus{
		lifecycle:    PluginMetaRestarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginMetaIdle,
		lastError:    err,
	}
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

// samePluginProcessStatus compares process status snapshots.
func samePluginProcessStatus(left, right pluginProcessStatus) bool {
	return left.lifecycle == right.lifecycle && left.availability == right.availability &&
		left.meta.lifecycle == right.meta.lifecycle && left.meta.availability == right.meta.availability &&
		left.meta.activity == right.meta.activity && errorText(left.meta.lastError) == errorText(right.meta.lastError)
}

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

// DeploymentWorkerLifecycle describes a deployment worker's lifecycle.
type DeploymentWorkerLifecycle string

const (
	DeploymentWorkerStarting   DeploymentWorkerLifecycle = "starting"
	DeploymentWorkerRunning    DeploymentWorkerLifecycle = "running"
	DeploymentWorkerRestarting DeploymentWorkerLifecycle = "restarting"
	DeploymentWorkerFailed     DeploymentWorkerLifecycle = "failed"
)

// deploymentWorkerStatus is the immutable worker snapshot sent to its pool.
type deploymentWorkerStatus struct {
	lifecycle    DeploymentWorkerLifecycle
	availability runtime.Availability
	meta         workerMetaStatus
}

// deploymentWorker owns one plugin worker meta-process incarnation.
type deploymentWorker[T Artifact] struct {
	act.Actor
	adapter    *Adapter[T]
	options    DeploymentWorkerOptions
	deployment Deployment
	workerMeta workerMetaState
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageDeploymentWorkerStatusChanged reports a worker status update to its pool.
type MessageDeploymentWorkerStatusChanged struct {
	worker gen.PID
	status deploymentWorkerStatus
}

// MessageDeploymentWorkerStopped reports worker shutdown to its pool.
type MessageDeploymentWorkerStopped struct {
	worker gen.PID
	pool   gen.PID
}

// MessageDeploymentWorkerRestartExhausted reports a terminal local recovery failure.
type MessageDeploymentWorkerRestartExhausted struct {
	err error
}

// MessageInvocationStarted marks an invocation accepted by a worker.
type MessageInvocationStarted struct{ callID uint64 }

// MessageInvocationFinished reports an invocation result to its manager.
type MessageInvocationFinished struct {
	callID uint64
	err    error
}

// MessageWorkerMetaRestart triggers a scheduled meta-process restart.
type MessageWorkerMetaRestart struct {
	token  uint64
	health bool
}

// MessageWorkerMetaHealthTick triggers a scheduled health check.
type MessageWorkerMetaHealthTick struct {
	alias gen.Alias
	token uint64
}

// MessageWorkerMetaHealthTimeout reports an unanswered health check.
type MessageWorkerMetaHealthTimeout struct {
	alias gen.Alias
	token uint64
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init configures retry state and starts the worker meta-process.
func (w *deploymentWorker[T]) Init(...any) error {
	w.options = deploymentWorkerOptionsWithDefaults(w.options)
	w.workerMeta.restart = runtime.NewScheduledBackoff(w.options.RetryMin, w.options.RetryMax)
	w.workerMeta.healthRestart = runtime.NewScheduledBackoff(w.options.RetryMin, w.options.RetryMax)
	w.workerMeta.status = workerMetaStatus{
		lifecycle:    WorkerMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginWorkerIdle,
	}
	return w.startWorkerMeta()
}

// Terminate cancels scheduled recovery and notifies the pool of shutdown.
func (w *deploymentWorker[T]) Terminate(error) {
	w.workerMeta.restart.CancelScheduled(false)
	w.workerMeta.healthRestart.CancelScheduled(false)
	_ = w.SendWithPriority(w.Parent(), MessageDeploymentWorkerStopped{worker: w.PID(), pool: w.Parent()}, gen.MessagePriorityHigh)
}

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

// HandleMessage processes worker lifecycle, health, and invocation messages.
func (w *deploymentWorker[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageInvokePlugin[T]:
		w.invoke(from, msg)

	case MessageWorkerMetaStartResult:
		if msg.alias != w.workerMeta.alias {
			return nil
		}
		if msg.err != nil {
			w.reportUnavailable(msg.err)
			w.cancelHealthCheck()
			w.workerMeta.alias = gen.Alias{}
			return w.scheduleWorkerMetaRestart(false)
		}
		if w.workerMeta.status.availability == runtime.AvailabilityReady {
			return nil
		}
		w.workerMeta.status = workerMetaStatus{
			lifecycle:    WorkerMetaRunning,
			availability: runtime.AvailabilityReady,
			activity:     PluginWorkerIdle,
		}
		w.scheduleHealthCheck(w.workerMeta.alias)
		w.publishStatus(DeploymentWorkerRunning)

	case MessageWorkerMetaRestart:
		restart := w.workerMeta.restart
		if msg.health {
			restart = w.workerMeta.healthRestart
		}
		if !restart.Pending || msg.token != restart.Token {
			return nil
		}
		restart.Pending = false
		restart.Cancel = nil
		return w.startWorkerMeta()

	case MessageWorkerMetaHealthTick:
		if w.workerMeta.status.availability != runtime.AvailabilityReady || w.workerMeta.pingPending || msg.alias != w.workerMeta.alias || msg.token != w.workerMeta.healthRestart.Token {
			return nil
		}
		w.workerMeta.pingPending = true
		_, err := w.SendAfter(w.PID(), MessageWorkerMetaHealthTimeout{alias: msg.alias, token: msg.token}, workerPingTimeout)
		if err != nil {
			w.workerMeta.pingPending = false
			w.retireWorkerMeta(msg.alias, fmt.Errorf("schedule plugin worker ping timeout: %w", err), true)
			return nil
		}
		if err := w.Send(msg.alias, MessageWorkerMetaPing{}); err != nil {
			w.retireWorkerMeta(msg.alias, fmt.Errorf("send plugin worker ping: %w", err), true)
		}

	case MessageWorkerMetaHealthTimeout:
		if !w.workerMeta.pingPending || msg.alias != w.workerMeta.alias || msg.token != w.workerMeta.healthRestart.Token {
			return nil
		}
		w.workerMeta.pingPending = false
		w.retireWorkerMeta(msg.alias, context.DeadlineExceeded, true)

	case MessageWorkerMetaPingResult:
		if !w.workerMeta.pingPending || msg.alias != w.workerMeta.alias {
			return nil
		}
		w.workerMeta.pingPending = false
		if msg.err != nil {
			w.retireWorkerMeta(msg.alias, fmt.Errorf("plugin worker ping: %w", msg.err), true)
			return nil
		}
		w.workerMeta.restart.CancelScheduled(true)
		w.workerMeta.healthRestart.CancelScheduled(true)
		w.scheduleHealthCheck(msg.alias)

	case MessageStop:
		return gen.TerminateReasonNormal

	case gen.MessageDownAlias:
		if msg.Alias != w.workerMeta.alias {
			return nil
		}
		healthFailure := w.workerMeta.pingPending
		w.cancelHealthCheck()
		w.reportUnavailable(msg.Reason)
		w.workerMeta.alias = gen.Alias{}
		return w.scheduleWorkerMetaRestart(healthFailure)
	}
	return nil
}

// HandleCall rejects unsupported synchronous worker calls.
func (w *deploymentWorker[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported deployment worker call %T", request), nil
}

// ---------------------------------------------------------------------------
// Invocation handling
// ---------------------------------------------------------------------------

// invoke executes one plugin invocation through the active meta-process.
func (w *deploymentWorker[T]) invoke(manager gen.PID, call MessageInvokePlugin[T]) {
	ctx := call.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, w.options.InvocationTimeout)
	defer cancel()
	_ = w.SendWithPriority(manager, MessageInvocationStarted{callID: call.CallID}, gen.MessagePriorityHigh)
	if err := ctx.Err(); err != nil {
		_ = w.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: err}, gen.MessagePriorityHigh)
		return
	}
	if call.Fn == nil {
		_ = w.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: fmt.Errorf("actorruntime: invocation function is required")}, gen.MessagePriorityHigh)
		return
	}
	if w.workerMeta.status.availability != runtime.AvailabilityReady || w.workerMeta.alias == (gen.Alias{}) {
		_ = w.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: runtime.ErrPluginUnavailable}, gen.MessagePriorityHigh)
		return
	}

	alias := w.workerMeta.alias
	w.workerMeta.status.activity = PluginWorkerBusy
	w.publishStatus(DeploymentWorkerRunning)
	response, err := w.CallWithTimeout(alias, workerInvokeCall[T]{context: ctx, fn: call.Fn}, callTimeoutSeconds(ctx, w.options.InvocationTimeout))
	w.workerMeta.status.activity = PluginWorkerIdle
	w.publishStatus(DeploymentWorkerRunning)
	recycle := err != nil
	if err == nil {
		result, ok := response.(workerInvokeResponse)
		if !ok {
			err = fmt.Errorf("actorruntime: unexpected plugin worker response %T", response)
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
		w.retireWorkerMeta(alias, err, true)
	}
	_ = w.SendWithPriority(manager, MessageInvocationFinished{callID: call.CallID, err: err}, gen.MessagePriorityHigh)
}

// ---------------------------------------------------------------------------
// Worker meta lifecycle
// ---------------------------------------------------------------------------

// startWorkerMeta creates and monitors the worker's meta-process.
func (w *deploymentWorker[T]) startWorkerMeta() error {
	if w.workerMeta.alias != (gen.Alias{}) {
		return nil
	}
	w.cancelHealthCheck()
	w.workerMeta.status = workerMetaStatus{
		lifecycle:    WorkerMetaStarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginWorkerIdle,
	}
	w.publishStatus(DeploymentWorkerStarting)
	alias, err := w.SpawnMeta(&workerMeta[T]{
		adapter:    w.adapter,
		deployment: w.deployment,
	}, gen.MetaOptions{})
	if err != nil {
		w.reportUnavailable(fmt.Errorf("spawn plugin worker: %w", err))
		return w.scheduleWorkerMetaRestart(false)
	}
	if err := w.MonitorAlias(alias); err != nil {
		_ = w.SendExitMeta(alias, gen.TerminateReasonShutdown)
		w.reportUnavailable(fmt.Errorf("monitor plugin worker: %w", err))
		return w.scheduleWorkerMetaRestart(false)
	}
	w.workerMeta.alias = alias
	return nil
}

// ---------------------------------------------------------------------------
// Worker meta recovery
// ---------------------------------------------------------------------------

// scheduleWorkerMetaRestart schedules normal or health recovery.
func (w *deploymentWorker[T]) scheduleWorkerMetaRestart(health bool) error {
	if w.workerMeta.status.lifecycle == WorkerMetaFailed {
		return nil
	}
	restart := w.workerMeta.restart
	if health {
		restart = w.workerMeta.healthRestart
	}
	if restart.Pending {
		return nil
	}
	delay := restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		w.failWorkerMeta(fmt.Errorf("plugin worker restart budget: %w", runtime.ErrBackoffStopped))
		return nil
	}
	restart.Token++
	token := restart.Token
	cancel, err := w.SendAfter(w.PID(), MessageWorkerMetaRestart{token: token, health: health}, delay)
	if err != nil {
		w.failWorkerMeta(fmt.Errorf("schedule plugin worker restart: %w", err))
		return nil
	}
	restart.Pending = true
	restart.Cancel = cancel
	return nil
}

// failWorkerMeta records terminal recovery failure and notifies the pool.
func (w *deploymentWorker[T]) failWorkerMeta(err error) {
	if w.workerMeta.status.lifecycle == WorkerMetaFailed {
		return
	}
	if w.workerMeta.status.lastError != nil {
		err = fmt.Errorf("%w: %v", err, w.workerMeta.status.lastError)
	}
	w.cancelHealthCheck()
	w.workerMeta.status = workerMetaStatus{
		lifecycle:    WorkerMetaFailed,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginWorkerIdle,
		lastError:    err,
	}
	w.publishStatus(DeploymentWorkerFailed)
	_ = w.SendWithPriority(w.Parent(), MessageDeploymentWorkerRestartExhausted{err: err}, gen.MessagePriorityHigh)
}

// retireWorkerMeta stops the active meta-process and begins recovery.
func (w *deploymentWorker[T]) retireWorkerMeta(alias gen.Alias, err error, health bool) {
	if alias != w.workerMeta.alias {
		return
	}
	w.cancelHealthCheck()
	w.reportUnavailable(err)
	w.workerMeta.alias = gen.Alias{}
	_ = w.SendExitMeta(alias, gen.TerminateReasonShutdown)
	_ = w.scheduleWorkerMetaRestart(health)
}

// ---------------------------------------------------------------------------
// Health monitoring
// ---------------------------------------------------------------------------

// scheduleHealthCheck queues a health check for the active meta-process.
func (w *deploymentWorker[T]) scheduleHealthCheck(alias gen.Alias) {
	if w.workerMeta.status.availability != runtime.AvailabilityReady || alias != w.workerMeta.alias {
		return
	}
	delay := w.options.HealthInterval
	w.workerMeta.healthRestart.Token++
	token := w.workerMeta.healthRestart.Token
	if _, err := w.SendAfter(w.PID(), MessageWorkerMetaHealthTick{alias: alias, token: token}, delay); err != nil {
		w.retireWorkerMeta(alias, fmt.Errorf("schedule plugin worker health check: %w", err), true)
	}
}

// cancelHealthCheck invalidates pending health checks.
func (w *deploymentWorker[T]) cancelHealthCheck() {
	w.workerMeta.healthRestart.Token++
	w.workerMeta.pingPending = false
}

// ---------------------------------------------------------------------------
// Status projection
// ---------------------------------------------------------------------------

// reportUnavailable records a recoverable meta-process failure.
func (w *deploymentWorker[T]) reportUnavailable(err error) {
	w.workerMeta.status = workerMetaStatus{
		lifecycle:    WorkerMetaRestarting,
		availability: runtime.AvailabilityUnavailable,
		activity:     PluginWorkerIdle,
		lastError:    err,
	}
	w.publishStatus(DeploymentWorkerRestarting)
}

// publishStatus sends the current worker status to its pool.
func (w *deploymentWorker[T]) publishStatus(lifecycle DeploymentWorkerLifecycle) {
	_ = w.SendWithPriority(w.Parent(), MessageDeploymentWorkerStatusChanged{
		worker: w.PID(),
		status: deploymentWorkerStatus{
			lifecycle:    lifecycle,
			availability: w.workerMeta.status.availability,
			meta:         w.workerMeta.status,
		},
	}, gen.MessagePriorityHigh)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// sameDeploymentWorkerStatus compares worker status snapshots.
func sameDeploymentWorkerStatus(left, right deploymentWorkerStatus) bool {
	return left.lifecycle == right.lifecycle && left.availability == right.availability &&
		left.meta.lifecycle == right.meta.lifecycle && left.meta.availability == right.meta.availability &&
		left.meta.activity == right.meta.activity && errorText(left.meta.lastError) == errorText(right.meta.lastError)
}

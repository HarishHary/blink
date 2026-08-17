package plugin

import (
	"context"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/runtime"
)

// workerMetaState tracks one worker's replaceable plugin meta-process.
type workerMetaState struct {
	alias         gen.Alias
	restart       *runtime.ScheduledBackoff
	healthRestart *runtime.ScheduledBackoff
	status        WorkerMetaStatus
	pingPending   bool
}

// DeploymentWorker owns one plugin worker meta-process incarnation.
// Its manager receives lifecycle and invocation facts directly; the pool only
// provides worker placement.
type DeploymentWorker[T Syncable] struct {
	act.Actor
	adapter    *Adapter[T]
	options    DeploymentWorkerOptions
	deployment Deployment
	manager    gen.PID
	meta       workerMetaState
}

// --- messages ---

type MessageDeploymentWorkerReady struct {
	worker gen.PID
	alias  gen.Alias
	pool   gen.PID
}

type MessageDeploymentWorkerUnavailable struct {
	worker gen.PID
	alias  gen.Alias
	pool   gen.PID
	err    error
}

type MessageDeploymentWorkerStopped struct {
	worker gen.PID
	pool   gen.PID
}

// MessageDeploymentWorkerRestartExhausted reports a terminal local recovery
// failure. Pool PID fences the fact to this worker's current pool incarnation.
type MessageDeploymentWorkerRestartExhausted struct {
	worker gen.PID
	pool   gen.PID
	health bool
	cause  error
}

// MessageWorkerMetaStarted and MessageDeploymentWorkerFinished bracket
// every invocation accepted by a worker, including unavailable workers.
type MessageWorkerMetaStarted struct {
	worker gen.PID
	callID uint64
	pool   gen.PID
}

type MessageWorkerMetaFinished struct {
	worker gen.PID
	callID uint64
	pool   gen.PID
	err    error
}

type MessageWorkerMetaRestart struct {
	token  uint64
	health bool
}

type MessageWorkerMetaHealthTick struct {
	alias gen.Alias
	token uint64
}

type MessageWorkerMetaHealthTimeout struct {
	alias gen.Alias
	token uint64
}

// --- messages ---

func (w *DeploymentWorker[T]) Init(...any) error {
	w.options = deploymentWorkerOptionsWithDefaults(w.options)
	w.meta.restart = runtime.NewScheduledBackoff(w.options.RetryMin, w.options.RetryMax)
	w.meta.healthRestart = runtime.NewScheduledBackoff(w.options.RetryMin, w.options.RetryMax)
	w.meta.status = WorkerMetaStatus{
		Lifecycle:    WorkerMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
		Activity:     PluginWorkerIdle,
	}
	return w.startWorkerMeta()
}

func (w *DeploymentWorker[T]) Terminate(error) {
	w.meta.restart.CancelScheduled(false)
	w.meta.healthRestart.CancelScheduled(false)
	w.notify(MessageDeploymentWorkerStopped{worker: w.PID(), pool: w.Parent()})
}

func (w *DeploymentWorker[T]) HandleMessage(_ gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageInvokePlugin[T]:
		w.invoke(msg)

	case MessageWorkerMetaStartResult:
		if msg.alias != w.meta.alias {
			return nil
		}
		if msg.err != nil {
			w.reportUnavailable(w.meta.alias, msg.err)
			w.cancelHealthCheck()
			w.meta.alias = gen.Alias{}
			return w.scheduleWorkerMetaRestart(false)
		}
		if w.meta.status.Availability == runtime.AvailabilityReady {
			return nil
		}
		w.meta.status = WorkerMetaStatus{
			Lifecycle:    WorkerMetaRunning,
			Availability: runtime.AvailabilityReady,
			Activity:     PluginWorkerIdle,
		}
		w.scheduleHealthCheck(w.meta.alias)
		w.notify(MessageDeploymentWorkerReady{worker: w.PID(), alias: w.meta.alias, pool: w.Parent()})

	case MessageWorkerMetaRestart:
		restart := w.meta.restart
		if msg.health {
			restart = w.meta.healthRestart
		}
		if !restart.Pending || msg.token != restart.Token {
			return nil
		}
		restart.Pending = false
		restart.Cancel = nil
		return w.startWorkerMeta()

	case MessageWorkerMetaHealthTick:
		if w.meta.status.Availability != runtime.AvailabilityReady || w.meta.pingPending || msg.alias != w.meta.alias || msg.token != w.meta.healthRestart.Token {
			return nil
		}
		w.meta.pingPending = true
		_, err := w.SendAfter(w.PID(), MessageWorkerMetaHealthTimeout{alias: msg.alias, token: msg.token}, workerPingTimeout)
		if err != nil {
			w.meta.pingPending = false
			w.retireWorkerMeta(msg.alias, fmt.Errorf("schedule plugin worker ping timeout: %w", err), true)
			return nil
		}
		if err := w.Send(msg.alias, MessageWorkerMetaPing{}); err != nil {
			w.retireWorkerMeta(msg.alias, fmt.Errorf("send plugin worker ping: %w", err), true)
		}

	case MessageWorkerMetaHealthTimeout:
		if !w.meta.pingPending || msg.alias != w.meta.alias || msg.token != w.meta.healthRestart.Token {
			return nil
		}
		w.meta.pingPending = false
		w.retireWorkerMeta(msg.alias, context.DeadlineExceeded, true)

	case MessageWorkerMetaPingResult:
		if !w.meta.pingPending || msg.alias != w.meta.alias {
			return nil
		}
		w.meta.pingPending = false
		if msg.err != nil {
			w.retireWorkerMeta(msg.alias, fmt.Errorf("plugin worker ping: %w", msg.err), true)
			return nil
		}
		w.meta.restart.CancelScheduled(true)
		w.meta.healthRestart.CancelScheduled(true)
		w.scheduleHealthCheck(msg.alias)

	case MessageStop:
		return gen.TerminateReasonNormal

	case gen.MessageDownAlias:
		if msg.Alias != w.meta.alias {
			return nil
		}
		alias := w.meta.alias
		healthFailure := w.meta.pingPending
		w.cancelHealthCheck()
		w.reportUnavailable(alias, msg.Reason)
		w.meta.alias = gen.Alias{}
		return w.scheduleWorkerMetaRestart(healthFailure)
	}
	return nil
}

func (w *DeploymentWorker[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported deployment worker call %T", request), nil
}

func (w *DeploymentWorker[T]) invoke(call MessageInvokePlugin[T]) {
	ctx := call.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, w.options.InvocationTimeout)
	defer cancel()
	w.notify(MessageWorkerMetaStarted{worker: w.PID(), callID: call.CallID, pool: w.Parent()})
	if err := ctx.Err(); err != nil {
		w.notify(MessageWorkerMetaFinished{worker: w.PID(), callID: call.CallID, pool: w.Parent(), err: err})
		return
	}
	if call.Fn == nil {
		w.notify(MessageWorkerMetaFinished{worker: w.PID(), callID: call.CallID, pool: w.Parent(), err: fmt.Errorf("actorruntime: invocation function is required")})
		return
	}
	if w.meta.status.Availability != runtime.AvailabilityReady || w.meta.alias == (gen.Alias{}) {
		w.notify(MessageWorkerMetaFinished{worker: w.PID(), callID: call.CallID, pool: w.Parent(), err: runtime.ErrPluginUnavailable})
		return
	}

	alias := w.meta.alias
	w.meta.status.Activity = PluginWorkerBusy
	response, err := w.CallWithTimeout(alias, workerInvokeCall[T]{context: ctx, fn: call.Fn}, callTimeoutSeconds(ctx, w.options.InvocationTimeout))
	w.meta.status.Activity = PluginWorkerIdle
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
	w.notify(MessageWorkerMetaFinished{worker: w.PID(), callID: call.CallID, pool: w.Parent(), err: err})
}

func (w *DeploymentWorker[T]) startWorkerMeta() error {
	if w.meta.alias != (gen.Alias{}) {
		return nil
	}
	w.cancelHealthCheck()
	w.meta.status = WorkerMetaStatus{
		Lifecycle:    WorkerMetaStarting,
		Availability: runtime.AvailabilityUnavailable,
		Activity:     PluginWorkerIdle,
	}
	alias, err := w.SpawnMeta(&workerMeta[T]{
		adapter:    w.adapter,
		deployment: w.deployment,
	}, gen.MetaOptions{})
	if err != nil {
		w.reportUnavailable(gen.Alias{}, fmt.Errorf("spawn plugin worker: %w", err))
		return w.scheduleWorkerMetaRestart(false)
	}
	if err := w.MonitorAlias(alias); err != nil {
		_ = w.SendExitMeta(alias, gen.TerminateReasonShutdown)
		w.reportUnavailable(alias, fmt.Errorf("monitor plugin worker: %w", err))
		return w.scheduleWorkerMetaRestart(false)
	}
	w.meta.alias = alias
	return nil
}

func (w *DeploymentWorker[T]) scheduleWorkerMetaRestart(health bool) error {
	if w.meta.status.Lifecycle == WorkerMetaFailed {
		return nil
	}
	restart := w.meta.restart
	if health {
		restart = w.meta.healthRestart
	}
	if restart.Pending {
		return nil
	}
	delay := restart.Strategy.NextBackOff()
	if delay == backoff.Stop {
		w.failWorkerMeta(health, fmt.Errorf("plugin worker restart budget: %w", runtime.ErrBackoffStopped))
		return nil
	}
	restart.Token++
	token := restart.Token
	cancel, err := w.SendAfter(w.PID(), MessageWorkerMetaRestart{token: token, health: health}, delay)
	if err != nil {
		w.failWorkerMeta(health, fmt.Errorf("schedule plugin worker restart: %w", err))
		return nil
	}
	restart.Pending = true
	restart.Cancel = cancel
	return nil
}

func (w *DeploymentWorker[T]) failWorkerMeta(health bool, err error) {
	if w.meta.status.Lifecycle == WorkerMetaFailed {
		return
	}
	if w.meta.status.LastError != nil {
		err = fmt.Errorf("%w: %v", err, w.meta.status.LastError)
	}
	w.cancelHealthCheck()
	w.meta.status = WorkerMetaStatus{
		Lifecycle:    WorkerMetaFailed,
		Availability: runtime.AvailabilityUnavailable,
		Activity:     PluginWorkerIdle,
		LastError:    err,
	}
	w.notify(MessageDeploymentWorkerRestartExhausted{worker: w.PID(), pool: w.Parent(), health: health, cause: err})
}

func (w *DeploymentWorker[T]) retireWorkerMeta(alias gen.Alias, err error, health bool) {
	if alias != w.meta.alias {
		return
	}
	w.cancelHealthCheck()
	w.reportUnavailable(alias, err)
	w.meta.alias = gen.Alias{}
	_ = w.SendExitMeta(alias, gen.TerminateReasonShutdown)
	_ = w.scheduleWorkerMetaRestart(health)
}

func (w *DeploymentWorker[T]) scheduleHealthCheck(alias gen.Alias) {
	if w.meta.status.Availability != runtime.AvailabilityReady || alias != w.meta.alias {
		return
	}
	delay := w.options.HealthInterval
	w.meta.healthRestart.Token++
	token := w.meta.healthRestart.Token
	if _, err := w.SendAfter(w.PID(), MessageWorkerMetaHealthTick{alias: alias, token: token}, delay); err != nil {
		w.retireWorkerMeta(alias, fmt.Errorf("schedule plugin worker health check: %w", err), true)
	}
}

func (w *DeploymentWorker[T]) cancelHealthCheck() {
	w.meta.healthRestart.Token++
	w.meta.pingPending = false
}

func (w *DeploymentWorker[T]) reportUnavailable(alias gen.Alias, err error) {
	if w.meta.status.Lifecycle == WorkerMetaRestarting {
		return
	}
	w.meta.status = WorkerMetaStatus{
		Lifecycle:    WorkerMetaRestarting,
		Availability: runtime.AvailabilityUnavailable,
		Activity:     PluginWorkerIdle,
		LastError:    err,
	}
	w.notify(MessageDeploymentWorkerUnavailable{worker: w.PID(), alias: alias, pool: w.Parent(), err: err})
}

func (w *DeploymentWorker[T]) notify(message any) {
	manager := w.manager
	if manager == (gen.PID{}) {
		manager = w.Parent()
	}
	_ = w.SendWithPriority(manager, message, gen.MessagePriorityHigh)
}

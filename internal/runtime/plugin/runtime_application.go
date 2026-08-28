// Package actorruntime provides the local Ergo runtime for Blink plugins.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"ergo.services/ergo/app"
	"ergo.services/ergo/gen"
	"golang.org/x/sync/semaphore"

	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// ---------------------------------------------------------------------------
// Runtime state
// ---------------------------------------------------------------------------

// applicationLifecycle tracks only caller-visible application boundaries.
type applicationLifecycle uint8

const (
	applicationNew applicationLifecycle = iota
	applicationRunning
	applicationStopping
	applicationTerminated
)

// runtimeCompletion pairs a broadcast completion signal with its durable result.
type runtimeCompletion struct {
	done chan struct{}
	err  error
}

// Application bridges external Go callers and snapshot updates into one
// runtimeSupervisor application member on a shared, process-owned Ergo node.
type Application[P Artifact, M any] struct {
	app.Application
	opts                ApplicationOptions
	logger              *logger.Logger
	lifecycle           applicationLifecycle
	supervisor          gen.PID
	mu                  sync.Mutex
	adapter             *Adapter[P]
	loader              Loader[M]
	productionAdmission *semaphore.Weighted
	shadowAdmission     *semaphore.Weighted
	nextCallID          uint64
	calls               outstandingCalls
	supervisorDone      runtimeCompletion
}

// outstandingCalls indexes the same accepted invocations two ways, under Application.mu.
type outstandingCalls struct {
	byID     map[uint64]*runtime.AsyncResult // results to complete on cancel and shutdown
	byPlugin map[string]int                  // per-plugin depth, for fair admission
}

// ---------------------------------------------------------------------------
// Application lifecycle
// ---------------------------------------------------------------------------

// NewApplication creates an unloaded plugin application.
func NewApplication[P Artifact, M any](opts ApplicationOptions, adapter *Adapter[P], loader Loader[M], logger *logger.Logger) *Application[P, M] {
	opts = runtimeOptionsWithDefaults(opts)
	return &Application[P, M]{
		opts:                opts,
		lifecycle:           applicationNew,
		adapter:             adapter,
		loader:              loader,
		logger:              logger,
		productionAdmission: semaphore.NewWeighted(int64(opts.maxOutstandingInvocations)),
		shadowAdmission:     semaphore.NewWeighted(int64(opts.shadowMaxOutstandingInvocations)),
		calls: outstandingCalls{
			byID:     make(map[uint64]*runtime.AsyncResult),
			byPlugin: make(map[string]int),
		},
		supervisorDone: runtimeCompletion{done: make(chan struct{})},
	}
}

// Name returns the distinct application name derived from the namespace this runtime follows.
func (a *Application[P, M]) Name() gen.Atom {
	return ApplicationName(a.opts.Namespace)
}

// SupervisorName returns the registered root supervisor name, derived the same way.
func (a *Application[P, M]) SupervisorName() gen.Atom { return SupervisorName(a.opts.Namespace) }

// Load describes the root runtime supervisor managed by Ergo.
func (a *Application[P, M]) Load(...any) (gen.ApplicationSpec, error) {
	supervisorOpts := a.opts.SupervisorOptions
	readerSet := supervisorOpts.SnapshotReader.Endpoint.Name != "" && supervisorOpts.SnapshotReader.ExecutorID != ""
	if a.opts.Namespace == "" || a.adapter == nil || a.logger == nil || supervisorOpts.Directory == "" || !readerSet || a.loader == nil || isNilLoader(a.loader) {
		return gen.ApplicationSpec{}, fmt.Errorf("namespace, directory, reader options, loader, adapter, and logger are required")
	}
	a.logger = a.logger.With("component", "plugin_runtime")
	return gen.ApplicationSpec{
		Name:        a.Name(),
		Description: fmt.Sprintf("Blink plugin runtime %s", a.SupervisorName()),
		Mode:        gen.ApplicationModePermanent,
		StopTimeout: a.opts.CloseTimeout,
		Network:     gen.ApplicationNetwork{RegisterTypes: snapshot.NetworkTypes()},
		Group: []gen.ApplicationMemberSpec{{
			Factory: func() gen.ProcessBehavior {
				return newRuntimeSupervisor(a.opts.Namespace, supervisorOpts, a.adapter, a.loader)
			},
		}},
		Map: map[string]gen.Atom{"supervisor": a.SupervisorName()},
	}, nil
}

// Init rejects restarting this single-use application behavior.
func (a *Application[P, M]) Init(gen.Ref, gen.ApplicationMode) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lifecycle != applicationNew {
		return fmt.Errorf("application %s cannot be restarted", a.Name())
	}
	return nil
}

// Start marks the application available after Ergo has started its group.
func (a *Application[P, M]) Start(gen.Ref, gen.ApplicationMode) {
	supervisor, err := a.Node().ProcessPID(a.SupervisorName())
	if err != nil {
		a.logger.ErrorF("lookup plugin runtime supervisor %s: %v", a.SupervisorName(), err)
		return
	}
	a.mu.Lock()
	if a.lifecycle == applicationNew {
		a.supervisor = supervisor
		a.lifecycle = applicationRunning
	}
	a.mu.Unlock()
}

// Stop closes admission and requests a bounded drain before Ergo stops members.
func (a *Application[P, M]) Stop(ref gen.Ref, _ error) {
	a.mu.Lock()
	if a.lifecycle != applicationRunning {
		a.mu.Unlock()
		return
	}
	a.lifecycle = applicationStopping
	a.mu.Unlock()

	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(int64(ref.Deadline()), 0))
	defer cancel()
	pid, err := a.Node().ProcessPID(a.SupervisorName())
	if err != nil {
		a.logger.ErrorF("lookup plugin runtime supervisor %s: %v", a.SupervisorName(), err)
		return
	}
	response, err := callPIDWithContext(ctx, a.Node(), pid, DrainRequest{}, 0)
	if err == nil {
		if reply, ok := response.(DrainResponse); !ok {
			err = fmt.Errorf("unexpected drain response %T", response)
		} else {
			err = reply.Err
		}
	}
	if err != nil {
		a.logger.ErrorF("drain plugin runtime %s: %v", a.SupervisorName(), err)
	}
}

// Terminate records final application completion and releases all caller-side calls.
func (a *Application[P, M]) Terminate(reason error) {
	a.mu.Lock()
	if a.lifecycle == applicationTerminated {
		a.mu.Unlock()
		return
	}
	pendingErr := runtime.ErrRuntimeStopped
	if a.lifecycle == applicationStopping {
		pendingErr = runtime.ErrPluginUnavailable
	}
	a.lifecycle = applicationTerminated
	a.supervisorDone.err = reason
	for _, call := range a.calls.byID {
		call.Complete(pendingErr)
	}
	close(a.supervisorDone.done)
	a.mu.Unlock()
}

// Wait blocks until the application exits or the context is canceled.
func (a *Application[P, M]) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	a.mu.Lock()
	if a.lifecycle == applicationNew {
		a.mu.Unlock()
		return runtime.ErrRuntimeNotStarted
	}
	done := a.supervisorDone.done
	a.mu.Unlock()

	select {
	case <-done:
		a.mu.Lock()
		defer a.mu.Unlock()
		if reason := a.supervisorDone.err; reason != nil && !errors.Is(reason, gen.TerminateReasonNormal) {
			return fmt.Errorf("%w: %v", runtime.ErrRuntimeStopped, reason)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Runtime status
// ---------------------------------------------------------------------------

// Status returns a live snapshot owned by the runtime supervisor.
func (a *Application[P, M]) Status(ctx context.Context) (SupervisorStatus, error) {
	if err := ctx.Err(); err != nil {
		return SupervisorStatus{}, err
	}

	a.mu.Lock()
	switch {
	case a.lifecycle == applicationTerminated:
		a.mu.Unlock()
		return SupervisorStatus{}, runtime.ErrRuntimeStopped
	case a.lifecycle == applicationNew:
		a.mu.Unlock()
		return SupervisorStatus{}, runtime.ErrRuntimeNotStarted
	}
	n, supervisor, done := a.Node(), a.supervisor, a.supervisorDone.done
	a.mu.Unlock()

	response, err := callPIDWithContext(ctx, n, supervisor, SupervisorStatusRequest{}, a.opts.SupervisorOptions.ControlTimeout)
	if err != nil {
		// The supervisor may have terminated between the liveness check and the
		// request. Prefer the runtime-level terminal error over a transport error.
		select {
		case <-done:
			return SupervisorStatus{}, runtime.ErrRuntimeStopped
		default:
		}
		return SupervisorStatus{}, err
	}

	status, ok := response.(SupervisorStatusResponse)
	if !ok {
		return SupervisorStatus{}, fmt.Errorf(
			"unexpected status response %T",
			response,
		)
	}
	return status.Status, nil
}

// State returns the typed snapshot state only after this runtime has committed
// and admitted the same generation to its catalog.
func (a *Application[P, M]) State(ctx context.Context) (snapshot.ProjectionState[M], error) {
	if err := ctx.Err(); err != nil {
		return snapshot.ProjectionState[M]{}, err
	}
	a.mu.Lock()
	if a.lifecycle == applicationTerminated {
		a.mu.Unlock()
		return snapshot.ProjectionState[M]{}, runtime.ErrRuntimeStopped
	}
	if a.lifecycle == applicationNew {
		a.mu.Unlock()
		return snapshot.ProjectionState[M]{}, runtime.ErrRuntimeNotStarted
	}
	n, supervisor, done := a.Node(), a.supervisor, a.supervisorDone.done
	a.mu.Unlock()
	response, err := callPIDWithContext(ctx, n, supervisor, SupervisorStateRequest{}, a.opts.SupervisorOptions.ControlTimeout)
	if err != nil {
		select {
		case <-done:
			return snapshot.ProjectionState[M]{}, runtime.ErrRuntimeStopped
		default:
		}
		return snapshot.ProjectionState[M]{}, err
	}
	metadata, ok := response.(SupervisorStateResponse)
	if !ok {
		return snapshot.ProjectionState[M]{}, fmt.Errorf("unexpected state response %T", response)
	}
	state, err := snapshot.NewProjectionClient[M](n, a.opts.Namespace).State(ctx)
	if err != nil {
		select {
		case <-done:
			return snapshot.ProjectionState[M]{}, runtime.ErrRuntimeStopped
		default:
		}
		return snapshot.ProjectionState[M]{}, err
	}
	if state.CommittedGeneration != metadata.Generation || !state.Availability.Routable() {
		return snapshot.ProjectionState[M]{}, runtime.ErrPluginUnavailable
	}
	return state, nil
}

// ---------------------------------------------------------------------------
// Invocation admission
// ---------------------------------------------------------------------------

// ErrShadowDropped means best-effort shadow admission was full. Production is
// unaffected and the shadow invocation was not sent into the actor tree.
var ErrShadowDropped = errors.New("shadow invocation dropped")

// CallBudget is how many invocations one caller call may split itself into for this rollout: the
// capacity the deployment declared, under the width the per-plugin admission share was built to hold.
func (a *Application[P, M]) CallBudget(rollout snapshot.Rollout) int {
	return min(rollout.Capacity(), max(1, a.opts.callFanOut))
}

// ---------------------------------------------------------------------------
// Invocation submission
// ---------------------------------------------------------------------------

// Submit admits and submits a production plugin invocation.
func (a *Application[P, M]) Submit(ctx context.Context, pluginID string, rolloutKey string, expectedGeneration int64, fn func(context.Context, P) error) (runtime.Invocation, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Invocation{}, err
	}
	if fn == nil {
		return runtime.Invocation{}, fmt.Errorf("invocation function is required")
	}
	if err := a.checkAccepting(); err != nil {
		return runtime.Invocation{}, err
	}
	// Reserve this plugin's share before the shared budget: a plugin whose processes are
	// stalled must fail its own calls fast instead of blocking every other plugin's
	// caller on the global semaphore until that caller's own deadline expires.
	a.mu.Lock()
	if a.calls.byPlugin[pluginID] >= a.opts.maxOutstandingInvocationsPerPlugin {
		a.mu.Unlock()
		return runtime.Invocation{}, runtime.ErrQueueFull
	}
	a.calls.byPlugin[pluginID]++
	a.mu.Unlock()
	releasePluginSlot := func() {
		a.mu.Lock()
		if a.calls.byPlugin[pluginID]--; a.calls.byPlugin[pluginID] <= 0 {
			delete(a.calls.byPlugin, pluginID)
		}
		a.mu.Unlock()
	}
	if err := a.productionAdmission.Acquire(ctx, 1); err != nil {
		releasePluginSlot()
		return runtime.Invocation{}, err
	}
	return a.submit(ctx, pluginID, rolloutKey, expectedGeneration, fn, false, func() {
		a.productionAdmission.Release(1)
		releasePluginSlot()
	})
}

// SubmitShadow uses an independent, non-blocking admission budget. When the
// shadow budget is full, the newest shadow invocation is dropped immediately.
// This prevents a slow experimental candidate from consuming production
// admission capacity or creating unbounded detached work.
func (a *Application[P, M]) SubmitShadow(ctx context.Context, pluginID string, expectedGeneration int64, fn func(context.Context, P) error) (runtime.Invocation, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Invocation{}, err
	}
	if fn == nil {
		return runtime.Invocation{}, fmt.Errorf("invocation function is required")
	}
	if err := a.checkAccepting(); err != nil {
		return runtime.Invocation{}, err
	}
	if !a.shadowAdmission.TryAcquire(1) {
		a.logger.ErrorF("shadow call %s dropped", pluginID)
		return runtime.Invocation{}, ErrShadowDropped
	}

	base := context.WithoutCancel(ctx)
	var shadowCtx context.Context
	var shadowCancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		shadowCtx, shadowCancel = context.WithDeadline(base, deadline)
	} else {
		shadowCtx, shadowCancel = context.WithCancel(base)
	}
	cleanup := func() {
		shadowCancel()
		a.shadowAdmission.Release(1)
	}
	invocation, err := a.submit(shadowCtx, pluginID, "", expectedGeneration, fn, true, cleanup)
	if err != nil && !errors.Is(err, ErrShadowDropped) {
		a.logger.ErrorF("shadow call %s: %v", pluginID, err)
	}
	return invocation, err
}

// checkAccepting reports whether the runtime accepts new invocations.
func (a *Application[P, M]) checkAccepting() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return applicationAcceptingError(a.lifecycle)
}

// applicationAcceptingError maps a lifecycle to the error a submission is refused with, nil while running.
func applicationAcceptingError(lifecycle applicationLifecycle) error {
	switch lifecycle {
	case applicationRunning:
		return nil
	case applicationTerminated:
		return runtime.ErrRuntimeStopped
	case applicationStopping:
		return runtime.ErrPluginUnavailable
	default:
		return runtime.ErrRuntimeNotStarted
	}
}

// submit sends an admitted invocation to the runtime supervisor.
func (a *Application[P, M]) submit(ctx context.Context, pluginID string, rolloutKey string, expectedGeneration int64, fn func(context.Context, P) error, shadow bool, cleanup func()) (runtime.Invocation, error) {
	invokeCtx, invokeCancel := context.WithCancel(ctx)
	release := sync.OnceFunc(func() {
		invokeCancel()
		if cleanup != nil {
			cleanup()
		}
	})
	fail := func(err error) (runtime.Invocation, error) {
		release()
		return runtime.Invocation{}, err
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	a.mu.Lock()
	if a.lifecycle != applicationRunning {
		lifecycle := a.lifecycle
		a.mu.Unlock()
		return fail(applicationAcceptingError(lifecycle))
	}
	a.nextCallID++
	callID := a.nextCallID
	nodeRef, supervisor := a.Node(), a.supervisor
	a.mu.Unlock()

	result := runtime.NewAsyncResult()
	state := runtime.NewInvocationState(func(err error) {
		invokeCancel()
		_ = nodeRef.SendWithPriority(supervisor, MessageCancelInvocation{CallID: callID, Err: err}, gen.MessagePriorityHigh)
	})

	a.mu.Lock()
	if a.lifecycle != applicationRunning {
		lifecycle := a.lifecycle
		a.mu.Unlock()
		return fail(applicationAcceptingError(lifecycle))
	}
	a.calls.byID[callID] = result
	a.mu.Unlock()

	request := MessageSubmitInvocation[P]{
		callID:             callID,
		context:            invokeCtx,
		cancel:             invokeCancel,
		pluginID:           pluginID,
		rolloutKey:         rolloutKey,
		expectedGeneration: expectedGeneration,
		fn:                 fn,
		shadow:             shadow,
		result:             result,
	}
	if err := nodeRef.Send(supervisor, request); err != nil {
		a.mu.Lock()
		delete(a.calls.byID, callID)
		a.mu.Unlock()
		return fail(fmt.Errorf("submit plugin invocation: %w", err))
	}

	stopContextWatch := context.AfterFunc(ctx, func() {
		state.RequestCancel(ctx.Err())
	})

	go func() {
		err := <-result.Ch
		stopContextWatch()
		a.mu.Lock()
		delete(a.calls.byID, callID)
		a.mu.Unlock()
		release()
		state.Complete(err)
		if shadow && err != nil {
			a.logger.ErrorF("shadow call %s: %v", pluginID, err)
		}
	}()

	return runtime.Invocation{Id: callID, State: state}, nil
}

// ---------------------------------------------------------------------------
// Control helpers
// ---------------------------------------------------------------------------

// controlOutcome holds the result of a control-plane request.
type controlOutcome struct {
	response any
	err      error
}

// callPIDWithContext calls an Ergo process while honoring the caller context.
func callPIDWithContext(ctx context.Context, n gen.Node, target gen.PID, request any, fallback time.Duration) (any, error) {
	result := make(chan controlOutcome, 1)
	go func() {
		response, err := n.CallPID(target, request, callTimeoutSeconds(ctx, fallback))
		result <- controlOutcome{response: response, err: err}
	}()

	select {
	case outcome := <-result:
		return outcome.response, outcome.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// callTimeoutSeconds returns the bounded timeout in whole seconds for Ergo calls.
func callTimeoutSeconds(ctx context.Context, fallback time.Duration) int {
	d := fallback
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < d || d <= 0 {
			d = remaining
		}
	}
	if d <= 0 {
		return 1
	}
	seconds := math.Ceil(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	maxInt := int(^uint(0) >> 1)
	if seconds > float64(maxInt) {
		return maxInt
	}
	return int(seconds)
}

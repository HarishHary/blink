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

	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

var (
	ErrRuntimeNotStarted = errors.New("actor runtime not started")
	ErrRuntimeStopped    = errors.New("actor runtime stopped")
)

// applicationLifecycle tracks only caller-visible application boundaries.
type applicationLifecycle string

const (
	applicationNew        applicationLifecycle = "new"
	applicationRunning    applicationLifecycle = "running"
	applicationStopping   applicationLifecycle = "stopping"
	applicationTerminated applicationLifecycle = "terminated"
)

// runtimeCompletion pairs a broadcast completion signal with its durable result.
type runtimeCompletion struct {
	done chan struct{}
	err  error
}

// Application bridges Go callers and snapshot updates into one plugin runtime on a shared node. It owns
// composition and lifecycle only: admission and invocation bookkeeping live in the gateway it loads.
type Application[P Artifact, M any] struct {
	app.Application
	opts           ApplicationOptions
	logger         *logger.Logger
	mu             sync.Mutex
	adapter        *Adapter[P]
	loader         Loader[M]
	supervisorDone runtimeCompletion
	lifecycle      applicationLifecycle
	err            error
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// NewApplication creates an unloaded plugin application.
func NewApplication[P Artifact, M any](opts ApplicationOptions, adapter *Adapter[P], loader Loader[M], logger *logger.Logger) *Application[P, M] {
	opts = runtimeOptionsWithDefaults(opts)
	return &Application[P, M]{
		opts:           opts,
		lifecycle:      applicationNew,
		adapter:        adapter,
		loader:         loader,
		logger:         logger,
		supervisorDone: runtimeCompletion{done: make(chan struct{})},
	}
}

// Name returns the distinct application name derived from the namespace this runtime follows.
func (a *Application[P, M]) Name() gen.Atom {
	return ApplicationName(a.opts.Namespace)
}

// SupervisorName returns the registered runtime supervisor name, derived the same way.
func (a *Application[P, M]) SupervisorName() gen.Atom { return SupervisorName(a.opts.Namespace) }

// GatewayName returns the registered invocation gateway name, derived the same way.
func (a *Application[P, M]) GatewayName() gen.Atom { return GatewayName(a.opts.Namespace) }

// PluginRuntimeName returns the registered branch supervisor name over both of them.
func (a *Application[P, M]) PluginRuntimeName() gen.Atom { return PluginRuntimeName(a.opts.Namespace) }

// Load describes the plugin runtime branch managed by Ergo: one gateway and the runtime it feeds.
func (a *Application[P, M]) Load(...any) (spec gen.ApplicationSpec, loadErr error) {
	defer func() { a.setErr(loadErr) }()
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
			// Ergo registers the name, as the branch supervisor's own child specs do for its children:
			// every name in the subtree is claimed by whoever spawns the process.
			Name: a.PluginRuntimeName(),
			Factory: func() gen.ProcessBehavior {
				return newPluginRuntimeSupervisor(a.opts, a.adapter, a.loader)
			},
		}},
		Map: map[string]gen.Atom{
			"runtime":    a.PluginRuntimeName(),
			"gateway":    a.GatewayName(),
			"supervisor": a.SupervisorName(),
		},
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
	// Resolving the name proves the branch came up. The PID is not kept: the runtime supervisor is the
	// second rest-for-one child, so it restarts alone, and this application is not restarted with it.
	if _, err := subtreePID(a.Node(), a.SupervisorName()); err != nil {
		a.setErr(err)
		a.logger.ErrorF("start plugin runtime: %v", err)
		return
	}
	a.mu.Lock()
	if a.lifecycle == applicationNew {
		a.lifecycle = applicationRunning
		a.err = nil
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
	// Admission closes first, so the drain below is not racing callers the gateway would still admit.
	if gateway, err := subtreePID(a.Node(), a.GatewayName()); err == nil {
		_ = a.Node().SendWithPriority(gateway, MessageDrain{}, gen.MessagePriorityHigh)
	}
	supervisor, err := subtreePID(a.Node(), a.SupervisorName())
	if err != nil {
		a.setErr(err)
		a.logger.ErrorF("drain plugin runtime: %v", err)
		return
	}
	response, err := callPIDWithContext(ctx, a.Node(), supervisor, DrainRequest{}, 0)
	if err == nil {
		if reply, ok := response.(DrainResponse); !ok {
			err = fmt.Errorf("unexpected drain response %T", response)
		} else {
			err = reply.Err
		}
	}
	if err != nil {
		a.setErr(err)
		a.logger.ErrorF("drain plugin runtime %s: %v", a.SupervisorName(), err)
	}
}

// Terminate records final application completion. Outstanding invocations are the gateway's, and its own
// Terminate has already failed them.
func (a *Application[P, M]) Terminate(reason error) {
	a.mu.Lock()
	if a.lifecycle == applicationTerminated {
		a.mu.Unlock()
		return
	}
	a.lifecycle = applicationTerminated
	a.supervisorDone.err = reason
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
		return ErrRuntimeNotStarted
	}
	done := a.supervisorDone.done
	a.mu.Unlock()

	select {
	case <-done:
		a.mu.Lock()
		defer a.mu.Unlock()
		if reason := a.supervisorDone.err; reason != nil && !errors.Is(reason, gen.TerminateReasonNormal) {
			return fmt.Errorf("%w: %w", ErrRuntimeStopped, reason)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// ErrShadowDropped means shadow admission was full; production is unaffected and nothing was sent.
var ErrShadowDropped = errors.New("shadow invocation dropped")

// CallBudget is how many invocations one call may split into: declared capacity under the admission
// share's width.
func (a *Application[P, M]) CallBudget(rollout snapshot.Rollout) int {
	return min(rollout.Capacity(), max(1, a.opts.callFanOut))
}

// Submit admits and submits a production plugin invocation through the gateway.
func (a *Application[P, M]) Submit(ctx context.Context, pluginID string, rolloutKey string, expectedGeneration int64, fn func(context.Context, P) error) (runtime.Invocation, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Invocation{}, err
	}
	if fn == nil {
		return runtime.Invocation{}, fmt.Errorf("invocation function is required")
	}
	client, err := a.gatewayClient()
	if err != nil {
		return runtime.Invocation{}, err
	}
	return client.submit(ctx, pluginID, rolloutKey, expectedGeneration, fn, false)
}

// SubmitShadow admits against the gateway's independent non-blocking budget, which drops the newest
// invocation when it is full, so a slow candidate cannot consume production capacity.
func (a *Application[P, M]) SubmitShadow(ctx context.Context, pluginID string, expectedGeneration int64, fn func(context.Context, P) error) (runtime.Invocation, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Invocation{}, err
	}
	if fn == nil {
		return runtime.Invocation{}, fmt.Errorf("invocation function is required")
	}
	client, err := a.gatewayClient()
	if err != nil {
		return runtime.Invocation{}, err
	}
	invocation, err := client.submit(ctx, pluginID, "", expectedGeneration, fn, true)
	switch {
	case errors.Is(err, ErrShadowDropped):
		a.logger.ErrorF("shadow call %s dropped", pluginID)
	case err != nil:
		a.logger.ErrorF("shadow call %s: %v", pluginID, err)
	}
	return invocation, err
}

// gatewayClient returns a client for this runtime's gateway, naming why a submission is refused while the
// application is not running.
func (a *Application[P, M]) gatewayClient() (gatewayClient[P], error) {
	a.mu.Lock()
	lifecycle := a.lifecycle
	a.mu.Unlock()
	switch lifecycle {
	case applicationRunning:
		return gatewayClient[P]{
			node:      a.Node(),
			namespace: a.opts.Namespace,
			opts:      a.opts.GatewayOptions,
		}, nil
	case applicationStopping:
		return gatewayClient[P]{}, ErrPluginUnavailable
	case applicationTerminated:
		return gatewayClient[P]{}, ErrRuntimeStopped
	default:
		return gatewayClient[P]{}, ErrRuntimeNotStarted
	}
}

// callSupervisor makes one control request to the runtime supervisor, resolving its name per request
// because the supervisor restarts alone under its branch and a PID kept from startup would address the
// incarnation before it. A failure once the application itself is finished reports that instead.
func (a *Application[P, M]) callSupervisor(ctx context.Context, request any) (any, error) {
	a.mu.Lock()
	n, done := a.Node(), a.supervisorDone.done
	a.mu.Unlock()

	var response any
	pid, err := subtreePID(n, a.SupervisorName())
	if err == nil {
		response, err = callPIDWithContext(ctx, n, pid, request, a.opts.SupervisorOptions.ControlTimeout)
	}
	if err != nil {
		select {
		case <-done:
			return nil, ErrRuntimeStopped
		default:
		}
		return nil, err
	}
	return response, nil
}

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

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// Status queries the supervisor's reconciled status without publishing gauges or propagating it.
// Application lifecycle is synchronized separately; health and its metrics belong to the supervisor.
func (a *Application[P, M]) Status(ctx context.Context) (result SupervisorStatus, statusErr error) {
	defer func() {
		if statusErr != nil {
			result.err = statusErr
		}
	}()
	if err := ctx.Err(); err != nil {
		return SupervisorStatus{}, err
	}

	a.mu.Lock()
	switch {
	case a.lifecycle == applicationTerminated:
		reason := a.supervisorDone.err
		a.mu.Unlock()
		if reason != nil {
			return SupervisorStatus{}, fmt.Errorf("%w: %w", ErrRuntimeStopped, reason)
		}
		return SupervisorStatus{}, ErrRuntimeStopped
	case a.lifecycle == applicationNew:
		reason := a.err
		a.mu.Unlock()
		if reason != nil {
			return SupervisorStatus{}, fmt.Errorf("%w: %w", ErrRuntimeNotStarted, reason)
		}
		return SupervisorStatus{}, ErrRuntimeNotStarted
	}
	a.mu.Unlock()

	response, err := a.callSupervisor(ctx, SupervisorStatusRequest{})
	if err != nil {
		return SupervisorStatus{}, err
	}

	status, ok := response.(SupervisorStatusResponse)
	if !ok {
		return SupervisorStatus{}, fmt.Errorf(
			"unexpected status response %T",
			response,
		)
	}
	a.mu.Lock()
	status.Status.err = runtime.FirstError(a.err, status.Status.err)
	a.mu.Unlock()
	return status.Status, nil
}

// setErr records lifecycle failures under the application lock.
func (a *Application[P, M]) setErr(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}

// State returns the typed snapshot state once this runtime committed and admitted that generation.
func (a *Application[P, M]) State(ctx context.Context) (snapshot.ProjectionState[M], error) {
	if err := ctx.Err(); err != nil {
		return snapshot.ProjectionState[M]{}, err
	}
	a.mu.Lock()
	if a.lifecycle == applicationTerminated {
		a.mu.Unlock()
		return snapshot.ProjectionState[M]{}, ErrRuntimeStopped
	}
	if a.lifecycle == applicationNew {
		a.mu.Unlock()
		return snapshot.ProjectionState[M]{}, ErrRuntimeNotStarted
	}
	n, done := a.Node(), a.supervisorDone.done
	a.mu.Unlock()
	response, err := a.callSupervisor(ctx, SupervisorStateRequest{})
	if err != nil {
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
			return snapshot.ProjectionState[M]{}, ErrRuntimeStopped
		default:
		}
		return snapshot.ProjectionState[M]{}, err
	}
	if state.CommittedGeneration != metadata.Generation || !state.Availability.Routable() {
		return snapshot.ProjectionState[M]{}, ErrPluginUnavailable
	}
	return state, nil
}

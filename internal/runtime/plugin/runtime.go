// Package actorruntime provides the local Ergo runtime for Blink plugins.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"

	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
)

// Options configures one plugin actor subtree on a process-owned Ergo node.
type Options[T plugin.Syncable] struct {
	// Name identifies this runtime subtree on the shared Ergo node. It is used
	// to derive deterministic child names such as "event-matcher-catalog".
	Name gen.Atom

	Directory     string
	SnapshotEvent gen.Event
	Adapter       *plugin.PluginAdapter[T]

	QueueSize int

	// MaxOutstandingInvocations bounds all production invocations accepted by
	// this runtime, including calls that are still waiting in deployment queues.
	// Admission waits for capacity and remains subject to the caller context.
	MaxOutstandingInvocations int

	// ShadowMaxOutstandingInvocations is an independent best-effort budget.
	// Shadow submissions are dropped immediately when this budget is exhausted.
	ShadowMaxOutstandingInvocations int

	DrainTimeout   time.Duration
	HealthInterval time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	ControlTimeout time.Duration
	CloseTimeout   time.Duration

	OnShadowError func(string, error)
	OnShadowDrop  func(string)
}

// Runtime bridges external Go callers and snapshot updates into one
// runtimeSupervisor subtree on a shared, process-owned Ergo node.
type Runtime[T plugin.Syncable] struct {
	node gen.Node
	deps actorDependencies[T]

	name           gen.Atom
	events         RuntimeEvents
	snapshotEvent  gen.Event
	directory      string
	controlTimeout time.Duration
	closeTimeout   time.Duration
	onShadowError  func(string, error)
	onShadowDrop   func(string)

	productionAdmission *admissionGate
	shadowAdmission     *admissionGate

	mu sync.Mutex

	supervisor gen.PID

	nextCallID atomic.Uint64
	pending    map[uint64]*runtime.AsyncResult

	started bool
	closing bool
	closed  bool

	rootDone   chan struct{}
	rootReason error
	closeDone  chan struct{}
	closeErr   error
}

func NewOnNode[T plugin.Syncable](n gen.Node, opts Options[T]) (*Runtime[T], error) {
	if n == nil {
		return nil, fmt.Errorf("actorruntime: node is required")
	}
	if opts.Name == "" || opts.SnapshotEvent.Name == "" || opts.Adapter == nil || opts.Directory == "" {
		return nil, fmt.Errorf("actorruntime: name, directory, snapshot event, and adapter are required")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 128
	}
	if opts.MaxOutstandingInvocations <= 0 {
		opts.MaxOutstandingInvocations = 512
	}
	if opts.ShadowMaxOutstandingInvocations <= 0 {
		opts.ShadowMaxOutstandingInvocations = 32
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 30 * time.Second
	}
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = 15 * time.Second
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = time.Minute
	}
	if opts.ControlTimeout <= 0 {
		opts.ControlTimeout = 30 * time.Second
	}
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = opts.DrainTimeout + 5*time.Second
	}

	deps := actorDependencies[T]{
		node:           n,
		adapter:        opts.Adapter,
		queueSize:      opts.QueueSize,
		drainTimeout:   opts.DrainTimeout,
		healthInterval: opts.HealthInterval,
		retryMin:       opts.RetryMin,
		retryMax:       opts.RetryMax,
	}

	events := RuntimeEventsFor(n, opts.Name)

	return &Runtime[T]{
		node:                n,
		deps:                deps,
		name:                opts.Name,
		events:              events,
		snapshotEvent:       opts.SnapshotEvent,
		directory:           opts.Directory,
		controlTimeout:      opts.ControlTimeout,
		closeTimeout:        opts.CloseTimeout,
		onShadowError:       opts.OnShadowError,
		onShadowDrop:        opts.OnShadowDrop,
		productionAdmission: newAdmissionGate(opts.MaxOutstandingInvocations),
		shadowAdmission:     newAdmissionGate(opts.ShadowMaxOutstandingInvocations),
		pending:             make(map[uint64]*runtime.AsyncResult),
	}, nil
}

func (r *Runtime[T]) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started {
		return nil
	}
	if r.closed {
		return fmt.Errorf("actorruntime: runtime cannot be restarted after Close")
	}

	supervisorExited := make(chan error, 1)
	r.rootDone = make(chan struct{})

	supervisor, err := r.node.Spawn(
		func() gen.ProcessBehavior {
			return newRuntimeSupervisor(runtimeSupervisorOptions[T]{
				Name:          r.name,
				Dependencies:  r.deps,
				SnapshotEvent: r.snapshotEvent,
				Directory:     r.directory,
				Stopped:       supervisorExited,
			})
		},
		gen.ProcessOptions{},
	)
	if err != nil {
		return fmt.Errorf("spawn runtime supervisor: %w", err)
	}

	r.supervisor = supervisor
	r.started = true
	go r.observeSupervisorExit(supervisor, supervisorExited, r.rootDone)
	return nil
}

func (r *Runtime[T]) observeSupervisorExit(pid gen.PID, exited <-chan error, rootDone chan struct{}) {
	reason := <-exited

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.supervisor != pid {
		return
	}

	r.rootReason = reason
	close(rootDone)

	pendingErr := ErrRuntimeStopped
	if r.closing {
		pendingErr = ErrPluginUnavailable
	}
	for _, pending := range r.pending {
		pending.Complete(pendingErr)
	}

	if r.closing {
		return
	}

	r.closing = true
	r.started = false
	r.closed = true
	r.closeErr = fmt.Errorf("%w: supervisor %v terminated: %v", ErrRuntimeStopped, pid, reason)
	if r.closeDone == nil {
		r.closeDone = make(chan struct{})
	}
	close(r.closeDone)
}

func (r *Runtime[T]) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	if !r.started && r.rootDone == nil {
		r.mu.Unlock()
		return ErrRuntimeNotStarted
	}
	rootDone := r.rootDone
	r.mu.Unlock()

	select {
	case <-rootDone:
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closeErr != nil {
			return r.closeErr
		}
		if r.rootReason != nil && !errors.Is(r.rootReason, gen.TerminateReasonNormal) {
			return fmt.Errorf("%w: %v", ErrRuntimeStopped, r.rootReason)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status returns a live snapshot owned by the runtime supervisor.
func (r *Runtime[T]) Status(ctx context.Context) (RuntimeStatus, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeStatus{}, err
	}

	r.mu.Lock()
	switch {
	case r.closed:
		r.mu.Unlock()
		return RuntimeStatus{}, ErrRuntimeStopped
	case !r.started:
		r.mu.Unlock()
		return RuntimeStatus{}, ErrRuntimeNotStarted
	}
	n, supervisor, rootDone := r.node, r.supervisor, r.rootDone
	r.mu.Unlock()

	response, err := callPIDWithContext(ctx, n, supervisor, runtimeGetStatus{}, r.controlTimeout)
	if err != nil {
		// The supervisor may have terminated between the liveness check and the
		// request. Prefer the runtime-level terminal error over a transport error.
		select {
		case <-rootDone:
			return RuntimeStatus{}, ErrRuntimeStopped
		default:
		}
		return RuntimeStatus{}, err
	}

	status, ok := response.(RuntimeStatus)
	if !ok {
		return RuntimeStatus{}, fmt.Errorf(
			"actorruntime: unexpected status response %T",
			response,
		)
	}
	return status, nil
}

// ErrShadowDropped means best-effort shadow admission was full. Production is
// unaffected and the shadow invocation was not sent into the actor tree.
var ErrShadowDropped = errors.New("shadow invocation dropped")

type admissionGate struct {
	tokens chan struct{}
}

func newAdmissionGate(limit int) *admissionGate {
	if limit <= 0 {
		limit = 1
	}
	return &admissionGate{tokens: make(chan struct{}, limit)}
}

func (g *admissionGate) acquire(ctx context.Context) error {
	select {
	case g.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *admissionGate) tryAcquire() bool {
	select {
	case g.tokens <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *admissionGate) release() {
	select {
	case <-g.tokens:
	default:
		panic("actorruntime: admission release without acquire")
	}
}

func (r *Runtime[T]) Submit(ctx context.Context, pluginID string, rolloutKey string, fn func(context.Context, T) error) (runtime.Invocation, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Invocation{}, err
	}
	if fn == nil {
		return runtime.Invocation{}, fmt.Errorf("actorruntime: invocation function is required")
	}
	if err := r.checkAccepting(); err != nil {
		return runtime.Invocation{}, err
	}
	if err := r.productionAdmission.acquire(ctx); err != nil {
		return runtime.Invocation{}, err
	}
	return r.submit(ctx, pluginID, rolloutKey, fn, false, r.productionAdmission.release)
}

// SubmitShadow uses an independent, non-blocking admission budget. When the
// shadow budget is full, the newest shadow invocation is dropped immediately.
// This prevents a slow experimental candidate from consuming production
// admission capacity or creating unbounded detached work.
func (r *Runtime[T]) SubmitShadow(ctx context.Context, pluginID string, fn func(context.Context, T) error) (runtime.Invocation, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Invocation{}, err
	}
	if fn == nil {
		return runtime.Invocation{}, fmt.Errorf("actorruntime: invocation function is required")
	}
	if err := r.checkAccepting(); err != nil {
		return runtime.Invocation{}, err
	}
	if !r.shadowAdmission.tryAcquire() {
		r.reportShadowDrop(pluginID)
		return runtime.Invocation{}, ErrShadowDropped
	}

	shadowCtx, shadowCancel := detachedContext(ctx)
	cleanup := func() {
		shadowCancel()
		r.shadowAdmission.release()
	}
	invocation, err := r.submit(shadowCtx, pluginID, "", fn, true, cleanup)
	if err != nil && !errors.Is(err, ErrShadowDropped) {
		r.reportShadowError(pluginID, err)
	}
	return invocation, err
}

func (r *Runtime[T]) checkAccepting() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || r.closing || r.closed {
		return ErrRuntimeNotStarted
	}
	return nil
}

func (r *Runtime[T]) submit(ctx context.Context, pluginID string, rolloutKey string, fn func(context.Context, T) error, shadow bool, cleanup func()) (runtime.Invocation, error) {
	fail := func(err error) (runtime.Invocation, error) {
		if cleanup != nil {
			cleanup()
		}
		return runtime.Invocation{}, err
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	r.mu.Lock()
	if !r.started || r.closing || r.closed {
		r.mu.Unlock()
		return fail(ErrRuntimeNotStarted)
	}
	nodeRef, supervisor := r.node, r.supervisor
	r.mu.Unlock()

	callID := r.nextCallID.Add(1)
	result := runtime.NewAsyncResult()
	state := runtime.NewInvocationState(func(err error) {
		_ = nodeRef.Send(supervisor, cancelCall{callID: callID, err: err})
	})

	r.mu.Lock()
	if !r.started || r.closing || r.closed {
		r.mu.Unlock()
		return fail(ErrRuntimeNotStarted)
	}
	r.pending[callID] = result
	r.mu.Unlock()

	request := runtimeSubmit[T]{
		callID:     callID,
		context:    ctx,
		pluginID:   pluginID,
		rolloutKey: rolloutKey,
		fn:         fn,
		shadow:     shadow,
		result:     result,
	}
	if err := nodeRef.Send(supervisor, request); err != nil {
		r.mu.Lock()
		delete(r.pending, callID)
		r.mu.Unlock()
		return fail(fmt.Errorf("submit plugin invocation: %w", err))
	}

	stopContextWatch := context.AfterFunc(ctx, func() {
		state.RequestCancel(ctx.Err())
	})

	go func() {
		err := <-result.Ch
		stopContextWatch()
		r.mu.Lock()
		delete(r.pending, callID)
		r.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		state.Complete(err)
		if shadow {
			r.reportShadowError(pluginID, err)
		}
	}()

	return runtime.Invocation{Id: callID, State: state}, nil
}

func (r *Runtime[T]) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.started {
		if r.closeDone == nil {
			err := r.closeErr
			r.mu.Unlock()
			return err
		}
		done := r.closeDone
		r.mu.Unlock()
		select {
		case <-done:
			r.mu.Lock()
			err := r.closeErr
			r.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if r.closing {
		done := r.closeDone
		r.mu.Unlock()
		select {
		case <-done:
			r.mu.Lock()
			err := r.closeErr
			r.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	r.closing = true
	r.closeDone = make(chan struct{})

	n := r.node
	supervisor := r.supervisor
	rootDone := r.rootDone
	r.mu.Unlock()

	closeErr := r.drainAndStop(ctx, n, supervisor, rootDone)

	r.mu.Lock()
	r.started = false
	r.closed = true
	if r.closeErr == nil {
		r.closeErr = closeErr
	} else if closeErr != nil {
		r.closeErr = errors.Join(r.closeErr, closeErr)
	}
	close(r.closeDone)
	err := r.closeErr
	r.mu.Unlock()
	return err
}

func (r *Runtime[T]) drainAndStop(ctx context.Context, n gen.Node, supervisor gen.PID, rootDone <-chan struct{}) error {
	response, err := callPIDWithContext(ctx, n, supervisor, drain{}, r.closeTimeout)
	if err == nil {
		reply, ok := response.(runtimeDrainReply)
		if !ok {
			err = fmt.Errorf("actorruntime: unexpected drain response %T", response)
		} else {
			err = reply.Err
		}
	}

	if err != nil {
		_ = n.SendExit(supervisor, gen.TerminateReasonShutdown)
	}

	select {
	case <-rootDone:
		return err
	case <-ctx.Done():
		_ = n.Kill(supervisor)
		return errors.Join(err, ctx.Err())
	}
}

func (r *Runtime[T]) reportShadowError(id string, err error) {
	if err == nil || r.onShadowError == nil {
		return
	}
	defer func() { _ = recover() }()
	r.onShadowError(id, err)
}

func (r *Runtime[T]) reportShadowDrop(id string) {
	if r.onShadowDrop == nil {
		return
	}
	defer func() { _ = recover() }()
	r.onShadowDrop(id)
}

type controlOutcome struct {
	response any
	err      error
}

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

func detachedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(base, deadline)
	}
	return context.WithCancel(base)
}

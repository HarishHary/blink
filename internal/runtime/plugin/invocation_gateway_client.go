package plugin

import (
	"context"
	"errors"
	"fmt"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

// gatewayClient submits invocations to one invocation gateway on behalf of Go callers. It holds no
// admission state of its own: every budget lives in the gateway, so a caller and the runtime cannot
// disagree about what was admitted.
type gatewayClient[T Artifact] struct {
	node      gen.Node
	namespace string
	opts      GatewayOptions
}

// submit admits one invocation through the gateway and returns the handle its caller cancels and waits on.
func (c gatewayClient[T]) submit(
	ctx context.Context,
	pluginID string,
	rolloutKey string,
	expectedGeneration int64,
	fn func(context.Context, T) error,
	shadow bool,
) (runtime.Invocation, error) {
	if c.node == nil {
		return runtime.Invocation{}, ErrRuntimeNotStarted
	}
	gateway, err := subtreePID(c.node, GatewayName(c.namespace))
	if err != nil {
		return runtime.Invocation{}, err
	}

	// The invocation's own context outlives the submission call, so cancelling the caller's handle stops
	// the plugin call whatever the gateway has done with it since.
	base, detach := invocationContext(ctx, shadow)
	invokeCtx, invokeCancel := context.WithCancel(base)
	release := func() { invokeCancel(); detach() }
	result := runtime.NewAsyncResult()
	response, err := callPIDWithContext(base, c.node, gateway, MessageGatewaySubmit[T]{
		Context:            invokeCtx,
		Cancel:             invokeCancel,
		PluginID:           pluginID,
		RolloutKey:         rolloutKey,
		ExpectedGeneration: expectedGeneration,
		Fn:                 fn,
		Shadow:             shadow,
		Result:             result,
	}, c.opts.SubmitTimeout)
	if err != nil {
		release()
		return runtime.Invocation{}, submitError(base, err)
	}
	reply, ok := response.(MessageGatewaySubmitted)
	if !ok {
		release()
		return runtime.Invocation{}, fmt.Errorf("unexpected submit response %T", response)
	}
	if reply.Err != nil {
		release()
		return runtime.Invocation{}, reply.Err
	}

	ref := reply.Ref
	state := runtime.NewInvocationState(func(err error) {
		invokeCancel()
		// Addressed to the gateway incarnation that minted the reference: a restarted gateway has already
		// failed this invocation, and its successor owns a different call of the same number.
		_ = c.node.SendWithPriority(ref.Gateway, MessageGatewayCancelInvocation{Ref: ref, Err: err}, gen.MessagePriorityHigh)
	})
	stopContextWatch := context.AfterFunc(base, func() {
		state.RequestCancel(base.Err())
	})
	go func() {
		err := <-result.Ch
		stopContextWatch()
		release()
		state.Complete(err)
	}()
	return runtime.Invocation{Id: ref.CallID, State: state}, nil
}

// invocationContext returns the context one invocation runs under and the release its completion owes. A
// shadow invocation is detached from its caller, so the production call that spawned it cannot cancel it,
// while its deadline still bounds it: a candidate is judged inside the same window.
func invocationContext(ctx context.Context, shadow bool) (context.Context, context.CancelFunc) {
	if !shadow {
		return ctx, func() {}
	}
	base := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(base, deadline)
	}
	return context.WithCancel(base)
}

// submitError names what refused a submission the gateway never answered. A call that ran out of time on
// the caller's own deadline is that deadline, not a runtime failure.
func submitError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, gen.ErrProcessUnknown) || errors.Is(err, gen.ErrProcessTerminated) {
		return fmt.Errorf("%w: %w", ErrPluginUnavailable, err)
	}
	return fmt.Errorf("submit plugin invocation: %w", err)
}

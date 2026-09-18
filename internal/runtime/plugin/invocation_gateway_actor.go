package plugin

import (
	"context"
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// GatewayLifecycle describes one invocation gateway incarnation, which is what it admits: Starting has no
// runtime to forward to yet, Draining and Stopped refuse.
type GatewayLifecycle string

const (
	GatewayStarting GatewayLifecycle = "starting"
	GatewayRunning  GatewayLifecycle = "running"
	GatewayDraining GatewayLifecycle = "draining"
	GatewayStopped  GatewayLifecycle = "stopped"
)

// InvocationRef identifies one admitted invocation together with the gateway incarnation that admitted it.
// Call ids restart at 1 with the gateway, so the PID is what keeps a completion meant for a previous
// incarnation off a new call holding the same number.
type InvocationRef struct {
	Gateway gen.PID
	CallID  uint64
}

// gatewayInvocation is one admitted invocation and the budgets it holds. Those permits outlive the
// caller's result: a cancelled or expired call frees admission only once the runtime reports the plugin
// stopped executing it.
type gatewayInvocation struct {
	pluginID  string
	shadow    bool
	result    *runtime.AsyncResult
	admitted  bool // the runtime accepted it and forwarded it into the catalog
	completed bool // its caller has a result, while its execution capacity may still be held
}

// gatewayWaiter is one caller waiting for a production permit to free. Its per-plugin permit is held
// already, so waiting cannot take a plugin past its own share.
type gatewayWaiter[T Artifact] struct {
	pid     gen.PID
	ref     gen.Ref
	request MessageGatewaySubmit[T]
}

// alive reports whether the waiting caller can still receive a response.
func (w gatewayWaiter[T]) alive() bool { return w.ref.IsAlive() }

// invocationGateway admits caller invocations into one plugin runtime and owns their bookkeeping: the
// budgets admission holds, the cancellations callers ask for, and the results and releases the runtime
// reports back. It is the runtime supervisor's sibling, not its child, so a lost runtime is a fact it
// survives to report to the callers waiting on it.
type invocationGateway[T Artifact] struct {
	act.Actor
	opts       GatewayOptions
	namespace  string
	runtimePID gen.PID // runtime supervisor incarnation last seen, re-resolved from its registered name
	watching   bool    // a monitor on that name is installed
	nextCallID uint64
	calls      map[uint64]*gatewayInvocation
	perPlugin  map[string]int // admitted and waiting invocations per plugin, for fair admission
	production int
	shadow     int
	waiters    []gatewayWaiter[T]
	lifecycle  GatewayLifecycle
	err        error
	labels     telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageGatewaySubmit asks the gateway to admit one invocation. Callers send it as a call, so a
// submission that has to wait for a permit waits in the gateway rather than in the caller's own budget.
type MessageGatewaySubmit[T Artifact] struct {
	Context            context.Context
	Cancel             context.CancelFunc
	PluginID           string
	RolloutKey         string
	ExpectedGeneration int64
	Fn                 func(context.Context, T) error
	Shadow             bool
	Result             *runtime.AsyncResult
}

// MessageGatewaySubmitted answers a submission with the reference the gateway admitted, or with the error
// that refused it and nothing held.
type MessageGatewaySubmitted struct {
	Ref InvocationRef
	Err error
}

// MessageGatewayCancelInvocation asks that one admitted invocation be cancelled. It frees no admission
// capacity by itself: the runtime reports the release when the plugin stops.
type MessageGatewayCancelInvocation struct {
	Ref InvocationRef
	Err error
}

// MessageGatewayInvocationAdmitted reports that the runtime took one invocation into its tree.
type MessageGatewayInvocationAdmitted struct{ Ref InvocationRef }

// MessageGatewayInvocationCompleted reports one invocation's result to the gateway that admitted it.
type MessageGatewayInvocationCompleted struct {
	Ref InvocationRef
	Err error
}

// MessageGatewayInvocationReleased reports that one invocation's execution capacity is free, which is what
// frees the admission permits the gateway held for it.
type MessageGatewayInvocationReleased struct{ Ref InvocationRef }

// MessageGatewayRadarTick drives the gateway's periodic gauge publish and runtime watch.
type MessageGatewayRadarTick struct{}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// newInvocationGateway creates an invocation gateway for one namespace.
func newInvocationGateway[T Artifact](namespace string, opts GatewayOptions, labels telemetry.Labels) gen.ProcessBehavior {
	return &invocationGateway[T]{namespace: namespace, opts: opts, labels: labels}
}

// Init checks the gateway name, allocates its books, and starts watching the runtime.
func (g *invocationGateway[T]) Init(...any) error {
	g.opts = gatewayOptionsWithDefaults(g.opts)
	if g.namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if err := requireSubtreeName(g, GatewayName(g.namespace)); err != nil {
		return fmt.Errorf("gateway name: %w", err)
	}
	g.calls = make(map[uint64]*gatewayInvocation)
	g.perPlugin = make(map[string]int)
	g.lifecycle = GatewayStarting
	g.ensureRuntimeWatch()
	// A message, not an inline publish: radar must not delay the first admission.
	if err := g.Send(g.PID(), MessageGatewayRadarTick{}); err != nil {
		return fmt.Errorf("schedule radar tick: %w", err)
	}
	return nil
}

// HandleCall admits submissions; a submission that has to wait is answered when a permit frees.
func (g *invocationGateway[T]) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	defer g.reconcileStatus()
	defer g.admitWaiters()
	switch m := request.(type) {
	case MessageGatewaySubmit[T]:
		return g.admit(from, ref, m)
	default:
		return fmt.Errorf("unsupported gateway call %T", request), nil
	}
}

// HandleMessage receives the runtime's invocation facts, caller cancellations, and lifecycle messages.
func (g *invocationGateway[T]) HandleMessage(from gen.PID, message any) error {
	defer g.reconcileStatus()
	defer g.admitWaiters()
	switch m := message.(type) {
	case MessageGatewayRadarTick:
		if from != g.PID() {
			return nil
		}
		g.ensureRuntimeWatch()
		if _, err := g.SendAfter(g.PID(), MessageGatewayRadarTick{}, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("reschedule radar tick: %w", err)
		}

	case MessageGatewayCancelInvocation:
		g.cancelInvocation(m)

	// Every runtime message below is fenced the same way: a reference this incarnation minted, sent by the
	// runtime itself. Call ids restart with the gateway, so an unfenced one could land on a live call.
	case MessageGatewayInvocationAdmitted:
		if m.Ref.Gateway != g.PID() || !g.isRuntime(from) {
			return nil
		}
		if call, tracked := g.calls[m.Ref.CallID]; tracked {
			call.admitted = true
		}

	case MessageGatewayInvocationCompleted:
		if m.Ref.Gateway != g.PID() || !g.isRuntime(from) {
			return nil
		}
		call, tracked := g.calls[m.Ref.CallID]
		if !tracked || call.completed {
			return nil
		}
		// The permits stay held: this result may be a cancellation the plugin has not finished acting on.
		call.completed = true
		call.result.Complete(m.Err)

	case MessageGatewayInvocationReleased:
		if m.Ref.Gateway != g.PID() || !g.isRuntime(from) {
			return nil
		}
		call, tracked := g.calls[m.Ref.CallID]
		if !tracked {
			return nil
		}
		// A release with no result before it means the runtime never reported one, and nobody else will.
		call.result.Complete(ErrPluginUnavailable)
		delete(g.calls, m.Ref.CallID)
		g.releaseBudgets(call.pluginID, call.shadow)

	case MessageDrain:
		if g.lifecycle != GatewayStarting && g.lifecycle != GatewayRunning {
			return nil
		}
		g.lifecycle = GatewayDraining
		g.refuseWaiters(ErrPluginUnavailable)

	case MessageStop:
		// Refused here rather than left to Terminate, so the pump below does not admit a caller into a
		// gateway that is already stopping.
		g.refuseWaiters(ErrRuntimeStopped)
		return gen.TerminateReasonNormal

	case gen.MessageDownProcessID:
		if m.ProcessID.Name != SupervisorName(g.namespace) {
			return nil
		}
		g.watching = false
		g.runtimePID = gen.PID{}
		g.err = fmt.Errorf("runtime supervisor down: %w", m.Reason)
		if g.lifecycle == GatewayRunning {
			g.lifecycle = GatewayStarting
		}
		g.failInFlightCalls(ErrPluginUnavailable)
		g.ensureRuntimeWatch()
	}
	return nil
}

// Terminate fails every invocation this incarnation owned: a gateway that is gone can neither report a
// result nor hold a permit, and no caller may be left waiting on either.
func (g *invocationGateway[T]) Terminate(reason error) {
	pendingErr := ErrRuntimeStopped
	if g.lifecycle == GatewayDraining {
		pendingErr = ErrPluginUnavailable
	}
	g.lifecycle = GatewayStopped
	g.err = runtime.FirstError(reason, g.err)
	g.refuseWaiters(pendingErr)
	for callID, call := range g.calls {
		delete(g.calls, callID)
		call.result.Complete(pendingErr)
	}
	g.production = 0
	g.shadow = 0
	g.perPlugin = make(map[string]int)
	g.reconcileStatus()
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// admit decides one submission: refused with nothing held, forwarded against a permit, or left waiting
// until production admission frees one. A refusal leaves the caller's result alone, since a submission the
// gateway did not take never reaches the runtime and its caller has the error in hand.
func (g *invocationGateway[T]) admit(from gen.PID, ref gen.Ref, request MessageGatewaySubmit[T]) (any, error) {
	if err := g.admissionError(); err != nil {
		g.labels.Count(g, metricGatewayRejected, "closed")
		return MessageGatewaySubmitted{Err: err}, nil
	}
	if request.Context == nil || request.Fn == nil || request.Result == nil {
		g.labels.Count(g, metricGatewayRejected, "invalid")
		return MessageGatewaySubmitted{Err: fmt.Errorf("invocation context, function, and result are required")}, nil
	}
	if err := request.Context.Err(); err != nil {
		g.labels.Count(g, metricGatewayRejected, "context")
		return MessageGatewaySubmitted{Err: err}, nil
	}
	if _, err := g.runtime(); err != nil {
		g.labels.Count(g, metricGatewayRejected, "unavailable")
		return MessageGatewaySubmitted{Err: err}, nil
	}

	// Shadow admission never waits: a slow candidate drops its own newest call rather than queueing, so it
	// cannot consume production capacity.
	if request.Shadow {
		if g.shadow >= g.opts.ShadowMaxOutstandingInvocations {
			g.labels.Count(g, metricGatewayRejected, "shadow_dropped")
			return MessageGatewaySubmitted{Err: ErrShadowDropped}, nil
		}
		g.shadow++
		return g.forward(request), nil
	}

	// This plugin's share comes before the shared budget, so one stalled plugin fails its own calls fast
	// rather than holding admission against every other caller.
	if g.perPlugin[request.PluginID] >= g.opts.MaxOutstandingInvocationsPerPlugin {
		g.labels.Count(g, metricGatewayRejected, "queue_full")
		return MessageGatewaySubmitted{Err: ErrQueueFull}, nil
	}
	g.perPlugin[request.PluginID]++
	if g.production >= g.opts.MaxOutstandingInvocations {
		if len(g.waiters) >= g.opts.MaxWaiting {
			g.releasePluginPermit(request.PluginID)
			g.labels.Count(g, metricGatewayRejected, "waiting_full")
			return MessageGatewaySubmitted{Err: ErrQueueFull}, nil
		}
		g.waiters = append(g.waiters, gatewayWaiter[T]{pid: from, ref: ref, request: request})
		g.labels.Count(g, metricGatewayWaited)
		// Answered from admitWaiters, or dropped there once the caller stops waiting.
		return nil, nil
	}
	g.production++
	return g.forward(request), nil
}

// forward hands one admitted invocation to the runtime supervisor and starts tracking it. Its permits are
// held already, so a hand-off that fails has to give them back.
func (g *invocationGateway[T]) forward(request MessageGatewaySubmit[T]) MessageGatewaySubmitted {
	target, err := g.runtime()
	if err != nil {
		g.releaseBudgets(request.PluginID, request.Shadow)
		g.labels.Count(g, metricGatewayRejected, "unavailable")
		return MessageGatewaySubmitted{Err: err}
	}
	g.nextCallID++
	ref := InvocationRef{Gateway: g.PID(), CallID: g.nextCallID}
	call := &gatewayInvocation{
		pluginID: request.PluginID,
		shadow:   request.Shadow,
		result:   request.Result,
	}
	g.calls[ref.CallID] = call
	submit := MessageSubmitInvocation[T]{
		ref:                ref,
		context:            request.Context,
		cancel:             request.Cancel,
		pluginID:           request.PluginID,
		rolloutKey:         request.RolloutKey,
		expectedGeneration: request.ExpectedGeneration,
		fn:                 request.Fn,
		shadow:             request.Shadow,
		result:             request.Result,
	}
	if err := g.Send(target, submit); err != nil {
		delete(g.calls, ref.CallID)
		g.releaseBudgets(call.pluginID, call.shadow)
		g.labels.Count(g, metricGatewayRejected, "unavailable")
		return MessageGatewaySubmitted{Err: fmt.Errorf("submit plugin invocation: %w", err)}
	}
	budget := "production"
	if request.Shadow {
		budget = "shadow"
	}
	g.labels.Count(g, metricGatewayAdmitted, budget)
	return MessageGatewaySubmitted{Ref: ref}
}

// admitWaiters admits waiting callers while production permits are free, oldest first. Every handler ends
// here, so nothing that frees a permit has to drain the queue itself.
func (g *invocationGateway[T]) admitWaiters() {
	for len(g.waiters) > 0 && g.production < g.opts.MaxOutstandingInvocations &&
		(g.lifecycle == GatewayStarting || g.lifecycle == GatewayRunning) {
		waiter := g.waiters[0]
		g.waiters = g.waiters[1:]
		if !waiter.alive() {
			// The caller stopped waiting, so nothing is owed but the per-plugin permit it held.
			g.releasePluginPermit(waiter.request.PluginID)
			continue
		}
		if err := waiter.request.Context.Err(); err != nil {
			g.releasePluginPermit(waiter.request.PluginID)
			g.labels.Count(g, metricGatewayRejected, "context")
			_ = g.SendResponse(waiter.pid, waiter.ref, MessageGatewaySubmitted{Err: err})
			continue
		}
		g.production++
		_ = g.SendResponse(waiter.pid, waiter.ref, g.forward(waiter.request))
	}
}

// cancelInvocation forwards one caller's cancellation to the runtime. The invocation stays tracked and its
// permits stay held: a caller giving up does not prove the plugin stopped.
func (g *invocationGateway[T]) cancelInvocation(message MessageGatewayCancelInvocation) {
	call, ok := g.calls[message.Ref.CallID]
	if !ok || message.Ref.Gateway != g.PID() || call.completed {
		return
	}
	target, err := g.runtime()
	if err != nil {
		g.err = fmt.Errorf("cancel invocation %d: %w", message.Ref.CallID, err)
		return
	}
	if err := g.SendWithPriority(target, message, gen.MessagePriorityHigh); err != nil {
		g.err = fmt.Errorf("cancel invocation %d: %w", message.Ref.CallID, err)
	}
}

// refuseWaiters answers every waiting caller once the gateway stops admitting.
func (g *invocationGateway[T]) refuseWaiters(err error) {
	waiters := g.waiters
	g.waiters = nil
	for _, waiter := range waiters {
		g.releasePluginPermit(waiter.request.PluginID)
		if waiter.alive() {
			g.labels.Count(g, metricGatewayRejected, "closed")
			_ = g.SendResponse(waiter.pid, waiter.ref, MessageGatewaySubmitted{Err: err})
		}
	}
}

// failInFlightCalls completes and forgets every in-flight invocation once the runtime can no longer account
// for them. A lost runtime takes its plugin processes with it, so nothing is still executing behind these
// permits.
func (g *invocationGateway[T]) failInFlightCalls(err error) {
	for callID, call := range g.calls {
		delete(g.calls, callID)
		call.result.Complete(err)
		g.releaseBudgets(call.pluginID, call.shadow)
	}
}

// releaseBudgets frees the budgets one invocation held. Admitting whoever was waiting on them is the
// handler's job, not this one's.
func (g *invocationGateway[T]) releaseBudgets(pluginID string, shadow bool) {
	if shadow {
		if g.shadow > 0 {
			g.shadow--
		}
		return
	}
	if g.production > 0 {
		g.production--
	}
	g.releasePluginPermit(pluginID)
}

// releasePluginPermit frees one plugin's share, forgetting a plugin that holds none.
func (g *invocationGateway[T]) releasePluginPermit(pluginID string) {
	if g.perPlugin[pluginID]--; g.perPlugin[pluginID] <= 0 {
		delete(g.perPlugin, pluginID)
	}
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// runtime resolves the runtime supervisor from its registered name, so a restarted runtime is
// addressed correctly without the gateway being told. Resolving it is what records the incarnation and
// opens admission.
func (g *invocationGateway[T]) runtime() (gen.PID, error) {
	pid, err := subtreePID(g.Node(), SupervisorName(g.namespace))
	if err != nil {
		return gen.PID{}, err
	}
	g.runtimePID = pid
	if g.lifecycle == GatewayStarting {
		g.lifecycle = GatewayRunning
		g.err = nil
	}
	g.ensureRuntimeWatch()
	return pid, nil
}

// isRuntime reports whether a message came from the runtime supervisor, re-resolving the name when the
// sender is not the incarnation last seen, since a runtime that started after the last submission is one
// the gateway has no PID for yet.
func (g *invocationGateway[T]) isRuntime(from gen.PID) bool {
	if from != (gen.PID{}) && from == g.runtimePID {
		return true
	}
	pid, err := g.runtime()
	return err == nil && pid == from
}

// ensureRuntimeWatch keeps a monitor on the runtime supervisor's name installed, which is what turns a
// lost runtime into a fact this gateway can report to the callers waiting on it.
func (g *invocationGateway[T]) ensureRuntimeWatch() {
	if g.watching {
		return
	}
	err := g.MonitorProcessID(gen.ProcessID{Name: SupervisorName(g.namespace), Node: g.Node().Name()})
	if err == nil || errors.Is(err, gen.ErrTargetExist) {
		g.watching = true
		return
	}
	g.Log().Debug("runtime supervisor monitor unavailable: namespace=%q error=%v", g.namespace, err)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// admissionError reports the error a submission is refused with, nil while the gateway admits.
func (g *invocationGateway[T]) admissionError() error {
	switch g.lifecycle {
	case GatewayDraining:
		return ErrPluginUnavailable
	case GatewayStopped:
		return ErrRuntimeStopped
	default:
		return nil
	}
}

// unreleasedCalls counts invocations whose caller has a result while their execution capacity is held.
func (g *invocationGateway[T]) unreleasedCalls() int {
	unreleased := 0
	for _, call := range g.calls {
		if call.completed {
			unreleased++
		}
	}
	return unreleased
}

// reconcileStatus publishes the gateway's own state without changing it. Nothing aggregates a gateway
// status, so its gauges are the whole of what it publishes.
func (g *invocationGateway[T]) reconcileStatus() {
	gatewayGauges{
		lifecycle:  g.lifecycle,
		production: g.production,
		shadow:     g.shadow,
		waiting:    len(g.waiters),
		unreleased: g.unreleasedCalls(),
	}.publish(g.labels, g)
}

// HandleInspect exposes the budgets in hand and how far the invocations behind them have progressed,
// which is what a permit held past its caller's result looks like from outside.
func (g *invocationGateway[T]) HandleInspect(gen.PID, ...string) map[string]string {
	admitted := 0
	for _, call := range g.calls {
		if call.admitted {
			admitted++
		}
	}
	return map[string]string{
		"gateway:err":              runtime.ErrorText(g.err),
		"gateway:lifecycle":        string(g.lifecycle),
		"gateway:runtime":          g.runtimePID.String(),
		"gateway:production":       fmt.Sprintf("%d/%d", g.production, g.opts.MaxOutstandingInvocations),
		"gateway:shadow":           fmt.Sprintf("%d/%d", g.shadow, g.opts.ShadowMaxOutstandingInvocations),
		"gateway:waiting":          fmt.Sprintf("%d/%d", len(g.waiters), g.opts.MaxWaiting),
		"gateway:plugins":          fmt.Sprintf("%d", len(g.perPlugin)),
		"gateway:in_flight_calls":  fmt.Sprintf("%d", len(g.calls)),
		"gateway:admitted_calls":   fmt.Sprintf("%d", admitted),
		"gateway:unreleased_calls": fmt.Sprintf("%d", g.unreleasedCalls()),
	}
}

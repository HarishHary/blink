package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// GatewayLifecycle is what a gateway admits: Starting has no runtime yet, Draining and Stopped refuse.
type GatewayLifecycle string

const (
	GatewayStarting GatewayLifecycle = "starting"
	GatewayRunning  GatewayLifecycle = "running"
	GatewayDraining GatewayLifecycle = "draining"
	GatewayStopped  GatewayLifecycle = "stopped"
)

// gatewayRuntimeWatchInterval is how long a gateway with no traffic can be wrong about its runtime.
const gatewayRuntimeWatchInterval = 30 * time.Second

// gatewayActorStatus is the gateway's summary of itself, the admission half of a namespace's readiness. Its
// full state stays with the gateway and is answered there.
type gatewayActorStatus struct {
	lifecycle    GatewayLifecycle
	availability runtime.Availability
	err          error
}

// gatewayActorState is the last report the branch supervisor accepted from its gateway.
type gatewayActorState struct {
	pid         gen.PID
	status      gatewayActorStatus
	statusEpoch int64
}

// InvocationRef identifies one admitted invocation. Call ids restart at 1 with each gateway, so the PID is
// what keeps a previous incarnation's completion off a live call holding the same number.
type InvocationRef struct {
	Gateway gen.PID
	CallID  uint64
}

// gatewayInvocation is one admitted invocation and the permits it holds: admitted once the runtime took it,
// completed once its caller has a result, and released — freeing the permits — once the plugin stopped.
type gatewayInvocation struct {
	pluginID  string
	shadow    bool
	result    *runtime.AsyncResult
	admitted  bool
	completed bool
}

// gatewayWaiter is one caller waiting for a production permit; its per-plugin permit is held already, so
// waiting cannot take a plugin past its share.
type gatewayWaiter[T Artifact] struct {
	pid     gen.PID
	ref     gen.Ref
	request MessageGatewaySubmit[T]
}

func (w gatewayWaiter[T]) alive() bool { return w.ref.IsAlive() }

// invocationGateway admits caller invocations into one plugin runtime and owns their bookkeeping. It is the
// runtime supervisor's sibling, not its child, so a lost runtime is a fact it survives to report.
type invocationGateway[T Artifact] struct {
	act.Actor
	opts            GatewayOptions
	namespace       string
	runtimePID      gen.PID
	watching        bool
	nextCallID      uint64
	calls           map[uint64]*gatewayInvocation
	perPlugin       map[string]int // counts waiting callers too, so waiting cannot take a plugin past its share
	production      int
	shadow          int
	waiters         []gatewayWaiter[T]
	lifecycle       GatewayLifecycle
	err             error
	labels          telemetry.Labels
	lastStatus      gatewayActorStatus // last published roll-up, the baseline propagateStatus dedupes against
	lastStatusEpoch int64
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageGatewaySubmit asks the gateway to admit one invocation. It is a call, so a submission that has to
// wait waits in the gateway rather than in the caller's own budget.
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

// MessageGatewaySubmitted answers a submission with the reference admitted, or the error that refused it.
type MessageGatewaySubmitted struct {
	Ref InvocationRef
	Err error
}

// MessageGatewayCancelInvocation asks that one invocation be cancelled; it frees no capacity by itself.
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

// MessageGatewayInvocationReleased reports one invocation's execution capacity free, which frees its permits.
type MessageGatewayInvocationReleased struct{ Ref InvocationRef }

// MessageGatewayWatchTick re-resolves the runtime, which is how a gateway with no traffic notices one.
type MessageGatewayWatchTick struct{}

// MessageGatewayStatusChanged publishes one gateway status revision to the branch supervisor, which is what
// puts a draining or runtime-less gateway into the namespace's readiness.
type MessageGatewayStatusChanged struct {
	pid         gen.PID
	status      gatewayActorStatus
	statusEpoch int64
}

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
	if err := g.Send(g.PID(), MessageGatewayWatchTick{}); err != nil {
		return fmt.Errorf("schedule watch tick: %w", err)
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
	case MessageGatewayWatchTick:
		if from != g.PID() {
			return nil
		}
		// Resolving, not just monitoring: a namespace nobody is calling still owes an honest readiness, and
		// the runtime it reached is what makes the gateway running.
		_, _ = g.runtime()
		if _, err := g.SendAfter(g.PID(), MessageGatewayWatchTick{}, gatewayRuntimeWatchInterval); err != nil {
			return fmt.Errorf("reschedule watch tick: %w", err)
		}

	case MessageGatewayCancelInvocation:
		g.cancelInvocation(m)

	// Each runtime message below is fenced twice: a reference this incarnation minted, from the runtime itself.
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
		// Permits stay held: this result may be a cancellation the plugin has not finished acting on.
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
		// A release with no result before it means nobody will ever report one.
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
		// Refused here, not in Terminate, so the trailing admit cannot take a caller into a stopping gateway.
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

// Terminate fails every invocation this incarnation owned: a gone gateway can neither answer nor hold a permit.
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

// admit decides one submission: refused holding nothing, forwarded against a permit, or left waiting for one.
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

	// Shadow never waits: a slow candidate drops its own newest call rather than consuming production capacity.
	if request.Shadow {
		if g.shadow >= g.opts.ShadowMaxOutstandingInvocations {
			g.labels.Count(g, metricGatewayRejected, "shadow_dropped")
			return MessageGatewaySubmitted{Err: ErrShadowDropped}, nil
		}
		g.shadow++
		return g.forward(request), nil
	}

	// The plugin's own share comes first, so one stalled plugin fails its own calls instead of everyone's.
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

// forward hands one admitted invocation to the runtime and tracks it, giving the permits back if that fails.
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

// admitWaiters admits waiting callers oldest first. Every handler ends here, so nothing that frees a permit
// has to drain the queue itself.
func (g *invocationGateway[T]) admitWaiters() {
	for len(g.waiters) > 0 && g.production < g.opts.MaxOutstandingInvocations &&
		(g.lifecycle == GatewayStarting || g.lifecycle == GatewayRunning) {
		waiter := g.waiters[0]
		g.waiters = g.waiters[1:]
		if !waiter.alive() {
			// Nobody to answer, so only the per-plugin permit is owed back.
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

// cancelInvocation forwards one caller's cancellation. Permits stay held: giving up does not prove the
// plugin stopped.
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

// failInFlightCalls completes and forgets every in-flight invocation. A lost runtime takes its plugin
// processes with it, so nothing is still executing behind these permits.
func (g *invocationGateway[T]) failInFlightCalls(err error) {
	for callID, call := range g.calls {
		delete(g.calls, callID)
		call.result.Complete(err)
		g.releaseBudgets(call.pluginID, call.shadow)
	}
}

// releaseBudgets frees the permits one invocation held; admitting whoever waited on them is the handler's job.
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

// runtime resolves the runtime supervisor by name, so a restart is addressed right without the gateway being
// told. Resolving it is also what records the incarnation and opens admission.
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

// isRuntime reports whether a message came from the runtime, re-resolving when the sender is not the
// incarnation last seen — a runtime started since the last submission has no PID here yet.
func (g *invocationGateway[T]) isRuntime(from gen.PID) bool {
	if from != (gen.PID{}) && from == g.runtimePID {
		return true
	}
	pid, err := g.runtime()
	return err == nil && pid == from
}

// ensureRuntimeWatch keeps a monitor on the runtime's name armed. It cannot arm at Init, where the gateway
// starts first and the name does not exist yet, so an unarmed watch is retried on every tick and submission.
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

// reconcileStatus publishes the gateway's own gauges, then its half of the namespace's readiness.
func (g *invocationGateway[T]) reconcileStatus() {
	gatewayGauges{
		lifecycle:  g.lifecycle,
		production: g.production,
		shadow:     g.shadow,
		waiting:    len(g.waiters),
		unreleased: g.unreleasedCalls(),
	}.publish(g.labels, g)
	g.propagateStatus()
}

// availability is the gateway's half of the namespace's readiness. A full budget is not degraded health: it is
// load, and readiness that flaps with load tells an operator nothing.
func (g *invocationGateway[T]) availability() runtime.Availability {
	if g.lifecycle != GatewayRunning {
		return runtime.AvailabilityUnavailable
	}
	return runtime.AvailabilityReady
}

// propagateStatus reports the gateway to the branch supervisor on a change, which is what makes a draining or
// runtime-less gateway show up in the namespace's readiness.
func (g *invocationGateway[T]) propagateStatus() {
	next := gatewayActorStatus{
		lifecycle:    g.lifecycle,
		availability: g.availability(),
		err:          g.err,
	}
	if next.lifecycle == g.lastStatus.lifecycle && next.availability == g.lastStatus.availability {
		return
	}
	g.lastStatus = next
	g.lastStatusEpoch = runtime.NextStatusEpoch(g.lastStatusEpoch)
	_ = g.SendWithPriority(g.Parent(), MessageGatewayStatusChanged{
		pid: g.PID(), status: next, statusEpoch: g.lastStatusEpoch,
	}, gen.MessagePriorityHigh)
}

// HandleInspect exposes the permits in hand and how far the invocations behind them have progressed.
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

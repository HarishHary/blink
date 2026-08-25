package plugin

import (
	"context"
	"fmt"
	"os/exec"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/runtime"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// PluginMetaLifecycle describes one plugin meta-process incarnation.
type PluginMetaLifecycle string

const (
	PluginMetaStarting   PluginMetaLifecycle = "starting"
	PluginMetaRunning    PluginMetaLifecycle = "running"
	PluginMetaRestarting PluginMetaLifecycle = "restarting"
	PluginMetaFailed     PluginMetaLifecycle = "failed"
)

// PluginMetaActivity labels how loaded one subprocess is rather than counting it: a process serves several invocations at once, so the label is what its owner publishes and the count rides along with it.
type PluginMetaActivity string

const (
	PluginMetaIdle PluginMetaActivity = "idle"
	PluginMetaBusy PluginMetaActivity = "busy"
	// PluginMetaSaturated means every declared slot is taken, so the next invocation dispatched there is refused; a subprocess serving one call at a time is saturated whenever it works at all, busy is the state only a larger capacity has, and neither is pressure - queue depth is where waiting shows up.
	PluginMetaSaturated PluginMetaActivity = "saturated"
)

const pluginMetaPingTimeout = 3 * time.Second

// pluginMetaCancelGrace is how long a cancelled or expired invocation may take to return before the subprocess is treated as hung, far longer than a cancelled gRPC call needs to unwind and far shorter than any caller deadline: a plugin honouring its RPC context returns within it and the failure stays call-local, while one that ignores cancellation can only be stopped by a kill that also ends the siblings running beside it (see failGenerationCalls).
const pluginMetaCancelGrace = time.Second

// pluginMetaState tracks one plugin process actor's replaceable meta-process.
type pluginMetaState struct {
	alias         gen.Alias
	generation    uint64
	restart       *runtime.ScheduledBackoff
	healthRestart *runtime.ScheduledBackoff
	status        pluginMetaStatus
	pingPending   bool
}

// pluginMetaStatus is owned by its plugin process actor.
type pluginMetaStatus struct {
	lifecycle    PluginMetaLifecycle
	availability runtime.Availability
	activity     PluginMetaActivity
	// inFlight and capacity are sampled whenever the activity label changes rather than published on their own: inFlight moves with every invocation and the manager schedules from the assignments it made itself, so a publish per call would walk the status chain for a number nothing reads.
	inFlight  int
	capacity  int
	lastError error
}

// pluginMetaSession is the atomically published plugin session.
type pluginMetaSession[T Artifact] struct {
	instance T
	client   *goplugin.Client
	rpc      RPC
}

// pluginProcessMeta owns one plugin subprocess and its RPC session.
type pluginProcessMeta[T Artifact] struct {
	gen.MetaProcess
	adapter    *Adapter[T]
	deployment Deployment
	runCtx     context.Context
	cancelRun  context.CancelFunc
	session    atomic.Pointer[pluginMetaSession[T]]
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessagePluginMetaStartResult reports whether the plugin session started.
type MessagePluginMetaStartResult struct {
	alias gen.Alias
	err   error
}

// MessagePluginMetaPing requests a plugin health check.
type MessagePluginMetaPing struct{}

// MessagePluginMetaPingResult reports a plugin health check result.
type MessagePluginMetaPingResult struct {
	alias gen.Alias
	err   error
}

// pluginMetaInvoke asks the meta-process to run one callback against its plugin session, as a message rather than a call so neither side blocks; the identifiers travel with it because the answer comes back as another message to be matched to the call that asked and the incarnation that served it.
type pluginMetaInvoke[T Artifact] struct {
	callID     uint64
	generation uint64
	context    context.Context
	fn         func(context.Context, T) error
}

// pluginMetaInvokeResult reports one invocation's outcome and recycle decision to the owning process actor, echoing the call and the incarnation that ran it.
type pluginMetaInvokeResult struct {
	alias      gen.Alias
	callID     uint64
	generation uint64
	err        error
	recycle    bool
}

// ---------------------------------------------------------------------------
// Plugin configuration
// ---------------------------------------------------------------------------

const pluginRetryPolicy = `{
  "methodConfig": [{
    "name": [{}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2.0,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`

// ---------------------------------------------------------------------------
// Meta lifecycle
// ---------------------------------------------------------------------------

// Init initializes the meta process and its lifecycle context.
func (m *pluginProcessMeta[T]) Init(process gen.MetaProcess) error {
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	return nil
}

// Start launches the plugin and blocks until shutdown completes.
func (m *pluginProcessMeta[T]) Start() error {
	instance, client, rpc, err := m.launchPlugin(m.runCtx)
	if err != nil {
		_ = m.Send(m.Parent(), MessagePluginMetaStartResult{alias: m.ID(), err: err})
		return fmt.Errorf("launch plugin process: %w", err)
	}

	m.session.Store(&pluginMetaSession[T]{instance: instance, client: client, rpc: rpc})

	if err := m.Send(m.Parent(), MessagePluginMetaStartResult{alias: m.ID()}); err != nil {
		m.close()
	}

	<-m.runCtx.Done()
	session := m.session.Swap(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if session.rpc != nil {
		_, _ = session.rpc.Shutdown(ctx, &emptypb.Empty{})
	}
	cancel()
	if session.client != nil {
		session.client.Kill()
	}
	return nil
}

// Terminate requests plugin session shutdown.
func (m *pluginProcessMeta[T]) Terminate(error) { m.close() }

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

// HandleMessage processes parent-issued plugin invocations and health checks.
func (m *pluginProcessMeta[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case pluginMetaInvoke[T]:
		if from != m.Parent() {
			return nil
		}
		m.invoke(msg)

	case MessagePluginMetaPing:
		if from != m.Parent() {
			return nil
		}
		session := m.session.Load()
		if session == nil || session.rpc == nil || m.runCtx.Err() != nil {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), pluginMetaPingTimeout)
		_, err := session.rpc.Ping(ctx, &emptypb.Empty{})
		cancel()
		_ = m.Send(m.Parent(), MessagePluginMetaPingResult{
			alias: m.ID(),
			err:   err,
		})
		if err != nil {
			m.close()
		}
	}
	return nil
}

// HandleCall rejects unsupported synchronous meta-process calls; invocations arrive as messages so that running one never blocks this meta-process.
func (m *pluginProcessMeta[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported plugin meta call %T", request), nil
}

// invoke validates one invocation and hands it to a goroutine so this meta-process returns to its mailbox while the plugin works, everything the answer needs travelling with that goroutine.
func (m *pluginProcessMeta[T]) invoke(msg pluginMetaInvoke[T]) {
	if msg.context == nil {
		m.answerInvocation(msg, pluginMetaInvokeResult{err: fmt.Errorf("actorruntime: invocation context is required")})
		return
	}
	if msg.fn == nil {
		m.answerInvocation(msg, pluginMetaInvokeResult{err: fmt.Errorf("actorruntime: invocation function is required")})
		return
	}
	session := m.session.Load()
	if session == nil || session.rpc == nil || m.runCtx.Err() != nil {
		m.answerInvocation(msg, pluginMetaInvokeResult{err: runtime.ErrPluginUnavailable})
		return
	}

	go func() {
		result := make(chan error, 1)
		go func() { result <- msg.fn(msg.context, session.instance) }()
		select {
		case err := <-result:
			m.answerInvocation(msg, m.classifyInvocation(msg.context, err))
		case <-msg.context.Done():
			// The caller cancelled or its deadline expired, neither of which implies an unhealthy subprocess: give the in-flight call the cancellation grace and classify what it returns as any other result, since only a callback still running after that grace is a hang.
			select {
			case err := <-result:
				m.answerInvocation(msg, m.classifyInvocation(msg.context, err))
			case <-time.After(pluginMetaCancelGrace):
				m.answerInvocation(msg, pluginMetaInvokeResult{err: msg.context.Err(), recycle: true})
			}
		}
	}()
}

// answerInvocation reports one outcome to the owning process actor and only then acts on a fatal one, since closing first would race this message against the meta-process's own DOWN and the owner would report a generic recycle instead of the failure that caused it.
func (m *pluginProcessMeta[T]) answerInvocation(msg pluginMetaInvoke[T], result pluginMetaInvokeResult) {
	result.alias, result.callID, result.generation = m.ID(), msg.callID, msg.generation
	_ = m.Send(m.Parent(), result)
	if result.recycle {
		m.close()
	}
}

// classifyInvocation decides whether one invocation's outcome is local to that call or fatal to the subprocess serving it: a transport that is gone takes the subprocess with it, and a cancellation or deadline is call-local whenever the caller's own context ended, since the same code without a finished caller context is the transport reporting its own failure.
func (m *pluginProcessMeta[T]) classifyInvocation(ctx context.Context, err error) pluginMetaInvokeResult {
	recycle := false
	if err != nil {
		switch errors.PluginErrorStatus(err).Code() {
		case codes.Unavailable:
			recycle = true
		case codes.Canceled, codes.DeadlineExceeded:
			recycle = ctx.Err() == nil
		}
	}
	return pluginMetaInvokeResult{err: err, recycle: recycle}
}

// HandleInspect exposes no custom meta-process metadata.
func (m *pluginProcessMeta[T]) HandleInspect(gen.PID, ...string) map[string]string { return nil }

// ---------------------------------------------------------------------------
// Plugin session management
// ---------------------------------------------------------------------------

// close idempotently signals the session to stop.
func (m *pluginProcessMeta[T]) close() {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// launchPlugin verifies, starts, and handshakes with the plugin subprocess.
func (m *pluginProcessMeta[T]) launchPlugin(ctx context.Context) (T, *goplugin.Client, RPC, error) {
	var zero T
	digest, err := helpers.BinaryChecksum(m.deployment.Path)
	if err != nil {
		return zero, nil, nil, fmt.Errorf("checksum plugin artifact %s: %w", m.deployment.Path, err)
	}
	if digest != m.deployment.Hash {
		return zero, nil, nil, fmt.Errorf("%w: %s expected %s, found %s", runtime.ErrArtifactMismatch, m.deployment.Path, m.deployment.Hash, digest)
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  handshake.ProtocolVersion,
			MagicCookieKey:   handshake.CookieKey,
			MagicCookieValue: m.adapter.MagicValue(),
		},
		Cmd:              exec.CommandContext(ctx, m.deployment.Path),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Plugins: map[string]goplugin.Plugin{
			m.adapter.PluginKey(): m.adapter.GRPCPlugin(),
		},
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithDefaultServiceConfig(pluginRetryPolicy),
		},
	})

	rawClient, err := client.Client()
	if err != nil {
		client.Kill()
		return zero, nil, nil, fmt.Errorf("connect: %w", err)
	}
	raw, err := rawClient.Dispense(m.adapter.PluginKey())
	if err != nil {
		client.Kill()
		return zero, nil, nil, fmt.Errorf("dispense: %w", err)
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	value, rpc, err := m.adapter.Handshake(handshakeCtx, raw, m.deployment)
	if err != nil {
		client.Kill()
		return zero, nil, nil, err
	}
	return value, client, rpc, nil
}

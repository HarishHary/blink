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

// PluginMetaActivity is deliberately separate from lifecycle.
type PluginMetaActivity string

const (
	PluginMetaIdle PluginMetaActivity = "idle"
	PluginMetaBusy PluginMetaActivity = "busy"
)

const pluginMetaPingTimeout = 3 * time.Second

// pluginMetaCancelGrace is how long a cancelled or expired invocation may take to return before the
// subprocess is treated as hung. A plugin that honours its RPC context returns within it, which is a
// call-local failure and must not cost the subprocess; one that ignores cancellation cannot be
// stopped any other way, because Go cannot kill the goroutine running an arbitrary callback. The
// grace is what separates those two cases, so it is deliberately far longer than a cancelled gRPC
// call needs to unwind and far shorter than any caller deadline.
const pluginMetaCancelGrace = time.Second

// pluginMetaCancelGraceSeconds is the grace rounded up for Ergo's whole-second call timeouts, plus
// one second for the same rounding in callTimeoutSeconds. The parent process actor waits this much
// longer than the caller's own deadline, so its own timeout never races the classification below.
const pluginMetaCancelGraceSeconds = int(pluginMetaCancelGrace/time.Second) + 1

// supportedCallsPerProcess is the invocation capacity one plugin process can actually serve today,
// whatever its deployment declares. HandleCall below blocks its meta-process for the whole
// invocation and the process actor that owns it takes one message at a time, so a second call reaches this
// subprocess only after the first returns: capacity is a property of this layer, not of the
// manager's arithmetic, and the manager clamps to it so a declared capacity never overstates what
// the runtime will run. Raising it belongs with the asynchronous invocation path that removes the
// serialization, not with the scheduler that would dispatch into it.
const supportedCallsPerProcess = 1

// pluginMetaState tracks one plugin process actor's replaceable meta-process.
type pluginMetaState struct {
	alias         gen.Alias
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
	lastError    error
}

// pluginMetaSession is the atomically published plugin session.
type pluginMetaSession[T Artifact] struct {
	instance T
	client   *goplugin.Client
	rpc      RPC
}

// pluginMeta owns one plugin subprocess and its RPC session.
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

// pluginMetaInvoke carries a synchronous callback without terminating the meta-process on plugin errors.
type pluginMetaInvoke[T Artifact] struct {
	context context.Context
	fn      func(context.Context, T) error
}

// pluginMetaInvokeResult returns the plugin result and recycle decision.
type pluginMetaInvokeResult struct {
	err     error
	recycle bool
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

// HandleMessage processes parent-issued plugin health checks.
func (m *pluginProcessMeta[T]) HandleMessage(from gen.PID, message any) error {
	switch message.(type) {
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

// HandleCall executes bounded plugin invocations from the parent process actor.
func (m *pluginProcessMeta[T]) HandleCall(from gen.PID, _ gen.Ref, request any) (any, error) {
	if from != m.Parent() {
		return pluginMetaInvokeResult{err: fmt.Errorf("actorruntime: unauthorized plugin process caller %s", from)}, nil
	}
	switch msg := request.(type) {
	case pluginMetaInvoke[T]:
		if msg.context == nil {
			return pluginMetaInvokeResult{err: fmt.Errorf("actorruntime: invocation context is required")}, nil
		}
		if msg.fn == nil {
			return pluginMetaInvokeResult{err: fmt.Errorf("actorruntime: invocation function is required")}, nil
		}
		session := m.session.Load()
		if session == nil || session.rpc == nil || m.runCtx.Err() != nil {
			return pluginMetaInvokeResult{err: runtime.ErrPluginUnavailable}, nil
		}

		result := make(chan error, 1)
		go func() { result <- msg.fn(msg.context, session.instance) }()
		select {
		case err := <-result:
			return m.classifyInvocation(msg.context, err), nil
		case <-msg.context.Done():
			// The caller cancelled or its deadline expired. Neither implies this subprocess is
			// unhealthy, so give the in-flight call the cancellation grace to unwind and classify
			// whatever it returns as any other result. Only a callback still running after that
			// grace is a hang, and killing the subprocess is the sole way to stop it.
			select {
			case err := <-result:
				return m.classifyInvocation(msg.context, err), nil
			case <-time.After(pluginMetaCancelGrace):
				m.close()
				return pluginMetaInvokeResult{err: msg.context.Err(), recycle: true}, nil
			}
		}
	}
	return fmt.Errorf("actorruntime: unsupported plugin meta call %T", request), nil
}

// classifyInvocation decides whether one invocation's outcome is local to that call or fatal to the
// subprocess serving it, and closes the session when it is fatal. A transport that is gone takes the
// subprocess with it, because nothing else can reach it. A cancellation or a deadline is call-local
// whenever the caller's own context ended, since the plugin answered a request that was withdrawn;
// the same code without a finished caller context is the transport reporting its own failure, and
// only then does the subprocess pay for it.
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
	if recycle {
		m.close()
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

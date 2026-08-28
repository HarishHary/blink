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

// PluginMetaActivity labels how loaded one subprocess is rather than counting it; the count rides along
// with the label its owner publishes.
type PluginMetaActivity string

const (
	PluginMetaIdle PluginMetaActivity = "idle"
	PluginMetaBusy PluginMetaActivity = "busy"
	// PluginMetaSaturated means every declared slot is taken, so the next invocation is refused; with a
	// capacity of one that is any work at all. Neither label is pressure; queue depth is.
	PluginMetaSaturated PluginMetaActivity = "saturated"
)

// pluginMetaPingTimeout is generous because the ping shares one gRPC server with real invocations, so a
// timeout near that scheduling delay would kill busy-but-healthy subprocesses.
const pluginMetaPingTimeout = 10 * time.Second

// pluginMetaCancelGrace is how long a cancelled invocation may take to return before the subprocess is
// hung: a plugin honouring its context unwinds well within it and stays call-local, while one that
// ignores cancellation is only stopped by a kill that ends its siblings too (see failGenerationCalls).
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
	// Sampled when the activity label changes, never published alone: a publish per invocation would
	// walk the status chain for a number nothing reads.
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

// pluginMetaInvoke asks the meta-process to run one callback against its session, as a message so
// neither side blocks; the identifiers travel with it because the answer is another message.
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
	return fmt.Errorf("unsupported plugin meta call %T", request), nil
}

// invoke validates one invocation and hands it to a goroutine, carrying everything the answer needs, so
// this meta-process returns to its mailbox while the plugin works.
func (m *pluginProcessMeta[T]) invoke(msg pluginMetaInvoke[T]) {
	if msg.context == nil {
		m.answerInvocation(msg, pluginMetaInvokeResult{err: fmt.Errorf("invocation context is required")})
		return
	}
	if msg.fn == nil {
		m.answerInvocation(msg, pluginMetaInvokeResult{err: fmt.Errorf("invocation function is required")})
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
			// A cancelled caller is no evidence of an unhealthy subprocess: allow the grace and classify
			// what returns as any other result, since only a callback outliving it is a hang.
			select {
			case err := <-result:
				m.answerInvocation(msg, m.classifyInvocation(msg.context, err))
			case <-time.After(pluginMetaCancelGrace):
				m.answerInvocation(msg, pluginMetaInvokeResult{err: msg.context.Err(), recycle: true})
			}
		}
	}()
}

// answerInvocation reports the outcome before acting on a fatal one, since closing first would race
// this message against the DOWN and the owner would report a generic recycle.
func (m *pluginProcessMeta[T]) answerInvocation(msg pluginMetaInvoke[T], result pluginMetaInvokeResult) {
	result.alias, result.callID, result.generation = m.ID(), msg.callID, msg.generation
	_ = m.Send(m.Parent(), result)
	if result.recycle {
		m.close()
	}
}

// classifyInvocation decides whether an outcome is call-local or fatal to the subprocess: a gone
// transport is fatal, and a cancellation is call-local only if the caller's own context ended.
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

// HandleInspect exposes which artifact this subprocess runs and whether it's connected
func (m *pluginProcessMeta[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"meta:deployment_id":   m.deployment.Id,
		"meta:deployment_name": m.deployment.Name,
		"meta:connected":       fmt.Sprintf("%t", m.session.Load() != nil),
		"meta:shutting_down":   fmt.Sprintf("%t", m.runCtx != nil && m.runCtx.Err() != nil),
	}
}

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

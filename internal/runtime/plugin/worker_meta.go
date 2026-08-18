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

// WorkerMetaLifecycle describes one worker meta-process incarnation.
type WorkerMetaLifecycle string

const (
	WorkerMetaStarting   WorkerMetaLifecycle = "starting"
	WorkerMetaRunning    WorkerMetaLifecycle = "running"
	WorkerMetaRestarting WorkerMetaLifecycle = "restarting"
	WorkerMetaFailed     WorkerMetaLifecycle = "failed"
)

// WorkerMetaActivity is deliberately separate from lifecycle.
type WorkerMetaActivity string

const (
	PluginWorkerIdle WorkerMetaActivity = "idle"
	PluginWorkerBusy WorkerMetaActivity = "busy"
)

const workerPingTimeout = 3 * time.Second

// workerMetaState tracks one worker's replaceable plugin meta-process.
type workerMetaState struct {
	alias         gen.Alias
	restart       *runtime.ScheduledBackoff
	healthRestart *runtime.ScheduledBackoff
	status        WorkerMetaStatus
	pingPending   bool
}

// WorkerMetaStatus is owned by its deployment worker.
type WorkerMetaStatus struct {
	Lifecycle    WorkerMetaLifecycle
	Availability runtime.Availability
	Activity     WorkerMetaActivity
	LastError    error
}

// workerMetaSession is the atomically published plugin session.
type workerMetaSession[T Syncable] struct {
	instance T
	client   *goplugin.Client
	rpc      RPC
}

// workerMeta owns one plugin subprocess and its RPC session.
type workerMeta[T Syncable] struct {
	gen.MetaProcess
	adapter    *Adapter[T]
	deployment Deployment
	runCtx     context.Context
	cancelRun  context.CancelFunc
	session    atomic.Pointer[workerMetaSession[T]]
}

// --- messages ---

// MessageWorkerMetaStartResult reports whether the plugin session started.
type MessageWorkerMetaStartResult struct {
	alias gen.Alias
	err   error
}

// MessageWorkerMetaPing requests a plugin health check.
type MessageWorkerMetaPing struct{}

// MessageWorkerMetaPingResult reports a plugin health check result.
type MessageWorkerMetaPingResult struct {
	alias gen.Alias
	err   error
}

// workerInvokeCall is the synchronous worker-to-meta callback contract.
// Plugin errors are returned in workerInvokeResponse so they do not
// terminate the meta-process.
type workerInvokeCall[T Syncable] struct {
	context context.Context
	fn      func(context.Context, T) error
}

// workerInvokeResponse returns the plugin result and recycle decision.
type workerInvokeResponse struct {
	err     error
	recycle bool
}

// --- messages ---

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

// Init initializes the meta process and its lifecycle context.
func (m *workerMeta[T]) Init(process gen.MetaProcess) error {
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	return nil
}

// Start launches the plugin and blocks until shutdown completes.
func (m *workerMeta[T]) Start() error {
	instance, client, rpc, err := m.launchPlugin(m.runCtx)
	if err != nil {
		_ = m.Send(m.Parent(), MessageWorkerMetaStartResult{alias: m.ID(), err: err})
		return fmt.Errorf("launch plugin worker: %w", err)
	}

	m.session.Store(&workerMetaSession[T]{instance: instance, client: client, rpc: rpc})

	if err := m.Send(m.Parent(), MessageWorkerMetaStartResult{alias: m.ID()}); err != nil {
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

// HandleMessage processes parent-issued plugin health checks.
func (m *workerMeta[T]) HandleMessage(from gen.PID, message any) error {
	switch message.(type) {
	case MessageWorkerMetaPing:
		if from != m.Parent() {
			return nil
		}
		session := m.session.Load()
		if session == nil || session.rpc == nil || m.runCtx.Err() != nil {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), workerPingTimeout)
		_, err := session.rpc.Ping(ctx, &emptypb.Empty{})
		cancel()
		_ = m.Send(m.Parent(), MessageWorkerMetaPingResult{
			alias: m.ID(),
			err:   err,
		})
		if err != nil {
			m.close()
		}
	}
	return nil
}

// HandleCall executes bounded plugin invocations from the parent worker.
func (m *workerMeta[T]) HandleCall(from gen.PID, _ gen.Ref, request any) (any, error) {
	if from != m.Parent() {
		return workerInvokeResponse{err: fmt.Errorf("actorruntime: unauthorized plugin worker caller %s", from)}, nil
	}
	switch msg := request.(type) {
	case workerInvokeCall[T]:
		if msg.context == nil {
			return workerInvokeResponse{err: fmt.Errorf("actorruntime: invocation context is required")}, nil
		}
		if msg.fn == nil {
			return workerInvokeResponse{err: fmt.Errorf("actorruntime: invocation function is required")}, nil
		}
		session := m.session.Load()
		if session == nil || session.rpc == nil || m.runCtx.Err() != nil {
			return workerInvokeResponse{err: runtime.ErrPluginUnavailable}, nil
		}

		result := make(chan error, 1)
		go func() { result <- msg.fn(msg.context, session.instance) }()
		select {
		case err := <-result:
			recycle := false
			if err != nil {
				switch errors.PluginErrorStatus(err).Code() {
				case codes.Unavailable:
					recycle = true
				case codes.Canceled, codes.DeadlineExceeded:
					recycle = msg.context.Err() == nil
				}
			}
			if recycle {
				m.close()
			}
			return workerInvokeResponse{err: err, recycle: recycle}, nil
		case <-msg.context.Done():
			// ponytail: an arbitrary callback can outlive cancellation; Go cannot kill it.
			m.close()
			return workerInvokeResponse{err: msg.context.Err(), recycle: true}, nil
		}
	}
	return fmt.Errorf("actorruntime: unsupported plugin worker call %T", request), nil
}

// Terminate requests plugin session shutdown.
func (m *workerMeta[T]) Terminate(error) { m.close() }

// HandleInspect exposes no custom worker metadata.
func (m *workerMeta[T]) HandleInspect(gen.PID, ...string) map[string]string { return nil }

// close idempotently signals the session to stop.
func (m *workerMeta[T]) close() {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// launchPlugin verifies, starts, and handshakes with the plugin subprocess.
func (m *workerMeta[T]) launchPlugin(ctx context.Context) (T, *goplugin.Client, RPC, error) {
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

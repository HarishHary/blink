package plugin

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"
)

// PluginWorkerLifecycle describes one worker meta-process incarnation.
type PluginWorkerLifecycle string

const (
	PluginWorkerStarting   PluginWorkerLifecycle = "starting"
	PluginWorkerRunning    PluginWorkerLifecycle = "running"
	PluginWorkerRestarting PluginWorkerLifecycle = "restarting"
	PluginWorkerDraining   PluginWorkerLifecycle = "draining"
	PluginWorkerStopped    PluginWorkerLifecycle = "stopped"
)

// PluginWorkerActivity is deliberately separate from lifecycle.
type PluginWorkerActivity string

const (
	PluginWorkerIdle PluginWorkerActivity = "idle"
	PluginWorkerBusy PluginWorkerActivity = "busy"
)

// PluginWorkerStatus is owned by deploymentPoolActor. The meta-process reports
// facts; its parent owns generation, restart policy, and the authoritative
// public status.
type PluginWorkerStatus struct {
	Lifecycle      PluginWorkerLifecycle
	Availability   runtime.Availability
	Activity       PluginWorkerActivity
	Generation     uint64
	RestartCount   uint64
	RestartPending bool
	InvocationID   uint64
	LastError      string
}

func (s PluginWorkerStatus) healthy() bool {
	return s.Lifecycle == PluginWorkerRunning && s.Availability.Routable()
}

type pluginWorkerMeta[T plugin.Syncable] struct {
	gen.MetaProcess

	deps             actorDependencies[T]
	deployment       deployment
	slot             int
	workerGeneration uint64

	runCtx    context.Context
	cancelRun context.CancelFunc
	stopCh    chan struct{}
	closeOnce sync.Once

	sessionMu sync.RWMutex
	instance  T
	client    *goplugin.Client
	rpc       plugin.PluginRPC
	ready     bool
}

type pluginWorkerStarted struct {
	slot             int
	alias            gen.Alias
	workerGeneration uint64
}

type pluginWorkerLaunchFailed struct {
	slot             int
	alias            gen.Alias
	workerGeneration uint64
	err              error
}

type pluginWorkerInvoke[T plugin.Syncable] struct {
	callID           uint64
	context          context.Context
	fn               func(context.Context, T) error
	workerGeneration uint64
	invocationID     uint64
}

type pluginWorkerInvocationFinished struct {
	slot             int
	alias            gen.Alias
	workerGeneration uint64
	invocationID     uint64
	callID           uint64
	err              error
	recycle          bool
}

type pluginWorkerPing struct{}

type pluginWorkerPingResult struct {
	slot             int
	alias            gen.Alias
	workerGeneration uint64
	err              error
}

type pluginWorkerStop struct{}

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

func (m *pluginWorkerMeta[T]) Init(process gen.MetaProcess) error {
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.stopCh = make(chan struct{})
	return nil
}

func (m *pluginWorkerMeta[T]) Start() error {
	instance, client, rpc, err := m.launchPlugin(m.runCtx)
	if err != nil {
		_ = m.Send(m.Parent(), pluginWorkerLaunchFailed{
			slot:             m.slot,
			alias:            m.ID(),
			workerGeneration: m.workerGeneration,
			err:              err,
		})
		return fmt.Errorf("launch plugin worker: %w", err)
	}

	m.sessionMu.Lock()
	m.instance, m.client, m.rpc, m.ready = instance, client, rpc, true
	m.sessionMu.Unlock()

	if err := m.Send(m.Parent(), pluginWorkerStarted{
		slot:             m.slot,
		alias:            m.ID(),
		workerGeneration: m.workerGeneration,
	}); err != nil {
		m.close()
	}

	<-m.stopCh
	m.shutdownSession()
	return nil
}

func (m *pluginWorkerMeta[T]) HandleMessage(_ gen.PID, message any) error {
	switch msg := message.(type) {
	case pluginWorkerInvoke[T]:
		if msg.workerGeneration != m.workerGeneration {
			m.reportInvocationFinished(msg, ErrPluginUnavailable, false)
			return nil
		}

		m.sessionMu.RLock()
		instance, ready := m.instance, m.ready
		m.sessionMu.RUnlock()
		if !ready {
			m.reportInvocationFinished(msg, ErrPluginUnavailable, false)
			return nil
		}

		err := msg.fn(msg.context, instance)
		recycle := shouldRecycle(err, msg.context)
		m.reportInvocationFinished(msg, err, recycle)
		if recycle {
			m.close()
		}

	case pluginWorkerPing:
		m.sessionMu.RLock()
		rpc, ready := m.rpc, m.ready
		m.sessionMu.RUnlock()
		if !ready || rpc == nil {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := rpc.Ping(ctx, &emptypb.Empty{})
		cancel()
		_ = m.Send(m.Parent(), pluginWorkerPingResult{
			slot:             m.slot,
			alias:            m.ID(),
			workerGeneration: m.workerGeneration,
			err:              err,
		})
		if err != nil {
			m.close()
		}

	case pluginWorkerStop:
		m.close()
	}
	return nil
}

func (m *pluginWorkerMeta[T]) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (m *pluginWorkerMeta[T]) Terminate(error) { m.close() }

func (m *pluginWorkerMeta[T]) HandleInspect(gen.PID, ...string) map[string]string {
	m.sessionMu.RLock()
	ready := m.ready
	m.sessionMu.RUnlock()
	return map[string]string{
		"slot":       fmt.Sprintf("%d", m.slot),
		"generation": fmt.Sprintf("%d", m.workerGeneration),
		"ready":      fmt.Sprintf("%t", ready),
	}
}

func (m *pluginWorkerMeta[T]) close() {
	m.closeOnce.Do(func() {
		if m.cancelRun != nil {
			m.cancelRun()
		}
		if m.stopCh != nil {
			close(m.stopCh)
		}
	})
}

func (m *pluginWorkerMeta[T]) reportInvocationFinished(msg pluginWorkerInvoke[T], err error, recycle bool) {
	_ = m.Send(m.Parent(), pluginWorkerInvocationFinished{
		slot:             m.slot,
		alias:            m.ID(),
		workerGeneration: m.workerGeneration,
		invocationID:     msg.invocationID,
		callID:           msg.callID,
		err:              err,
		recycle:          recycle,
	})
}

func (m *pluginWorkerMeta[T]) launchPlugin(ctx context.Context) (T, *goplugin.Client, plugin.PluginRPC, error) {
	var zero T
	if err := m.validateArtifact(); err != nil {
		return zero, nil, nil, err
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  handshake.ProtocolVersion,
			MagicCookieKey:   handshake.CookieKey,
			MagicCookieValue: m.deps.adapter.MagicValue(),
		},
		Cmd:              exec.CommandContext(ctx, m.deployment.path),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Plugins: map[string]goplugin.Plugin{
			m.deps.adapter.PluginKey(): m.deps.adapter.GRPCPlugin(),
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
	raw, err := rawClient.Dispense(m.deps.adapter.PluginKey())
	if err != nil {
		client.Kill()
		return zero, nil, nil, fmt.Errorf("dispense: %w", err)
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	value, rpc, err := m.deps.adapter.Handshake(handshakeCtx, raw, m.deployment.path, m.deployment.hash)
	if err != nil {
		client.Kill()
		return zero, nil, nil, err
	}
	return value, client, rpc, nil
}

func (m *pluginWorkerMeta[T]) validateArtifact() error {
	digest, err := helpers.BinaryChecksum(m.deployment.path)
	if err != nil {
		return fmt.Errorf("checksum plugin artifact %s: %w", m.deployment.path, err)
	}
	if digest != m.deployment.hash {
		return fmt.Errorf("%w: %s expected %s, found %s", ErrArtifactMismatch, m.deployment.path, m.deployment.hash, digest)
	}
	return nil
}

func (m *pluginWorkerMeta[T]) shutdownSession() {
	m.sessionMu.Lock()
	rpc, client := m.rpc, m.client
	m.ready = false
	m.rpc, m.client = nil, nil
	m.sessionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if rpc != nil {
		_, _ = rpc.Shutdown(ctx, &emptypb.Empty{})
	}
	cancel()
	if client != nil {
		client.Kill()
	}
}

func shouldRecycle(err error, ctx context.Context) bool {
	if err == nil {
		return false
	}
	switch errors.PluginErrorStatus(err).Code() {
	case codes.Unavailable:
		return true
	case codes.Canceled, codes.DeadlineExceeded:
		return ctx.Err() == nil
	default:
		return false
	}
}

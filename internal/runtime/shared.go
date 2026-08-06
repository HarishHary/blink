package runtime

import (
	"context"
	"errors"
	"time"

	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
)

var (
	ErrPluginUnavailable = errors.New("plugin unavailable")
	ErrQueueFull         = errors.New("plugin queue full")
	ErrArtifactMismatch  = errors.New("plugin artifact checksum mismatch")
	ErrArtifactScan      = errors.New("plugin artifact scan failed")
	ErrArtifactWatch     = errors.New("plugin artifact watch failed")
	ErrArtifactResolve   = errors.New("plugin artifact resolution failed")
	ErrSnapshotLoad      = errors.New("snapshot state load failed")
	ErrSnapshotPublish   = errors.New("snapshot publication failed")
	ErrSnapshotRead      = errors.New("snapshot read failed")
	ErrSnapshotSubscribe = errors.New("snapshot event subscription failed")
	ErrRuntimeNotStarted = errors.New("actor runtime not started")
	ErrRuntimeStopped    = errors.New("actor runtime stopped")
	ErrWorkerRecycle     = errors.New("worker recycled after plugin transport failure")
	ErrWorkerUnhealthy   = errors.New("worker plugin health check failed")
)

type ActorDependencies[T plugin.Syncable] struct {
	Node           gen.Node
	Adapter        *plugin.PluginAdapter[T]
	QueueSize      int
	DrainTimeout   time.Duration
	HealthInterval time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
}

type Deployment struct {
	plugin.BinaryState
	Path       string
	Hash       string
	RolloutPct float64
}

type DeploymentPoolKey struct {
	pools.PoolKey
	MaxProcs int
}

func (d *Deployment) PoolKey() DeploymentPoolKey {
	return DeploymentPoolKey{
		PoolKey:  pools.PoolKey{Id: d.Id, Name: d.Name, Hash: d.Hash},
		MaxProcs: d.WorkerCount(),
	}
}

func (d *Deployment) WorkerCount() int {
	return max(1, d.MaxProcs)
}

type MessageDrain struct{}
type MessageStop struct{}

type ScheduledBackoff struct {
	Strategy *backoff.ExponentialBackOff
	Pending  bool
	Token    uint64
	Cancel   gen.CancelFunc
}

func NewScheduledBackoff(minDelay, maxDelay time.Duration) *ScheduledBackoff {
	return &ScheduledBackoff{
		Strategy: backoff.NewExponentialBackOff(
			backoff.WithInitialInterval(minDelay),
			backoff.WithMaxInterval(maxDelay),
			backoff.WithMultiplier(2),
			backoff.WithMaxElapsedTime(0),
		),
	}
}

func (s *ScheduledBackoff) CancelScheduled(reset bool) {
	if s == nil {
		return
	}
	if s.Cancel != nil {
		s.Cancel()
		s.Cancel = nil
	}
	s.Pending = false
	s.Token++
	if reset {
		s.Strategy.Reset()
	}
}

// Cross-component invocation messages.
type MessageInvokePlugin[T plugin.Syncable] struct {
	CallID     uint64
	Context    context.Context
	Cancel     context.CancelFunc
	PluginID   string
	RolloutKey string
	Fn         func(context.Context, T) error
	Shadow     bool
}

type MessageCancelInvocation struct {
	CallID uint64
	Err    error
}

type MessageInvocationCompleted struct {
	CallID uint64
	Err    error
}

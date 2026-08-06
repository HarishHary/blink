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
	ErrSnapshotLoad      = errors.New("snapshot state load failed")
	ErrSnapshotPublish   = errors.New("snapshot publication failed")
	ErrRuntimeNotStarted = errors.New("actor runtime not started")
	ErrRuntimeStopped    = errors.New("actor runtime stopped")
	ErrWorkerRecycle     = errors.New("worker recycled after plugin transport failure")
	ErrWorkerUnhealthy   = errors.New("worker plugin health check failed")
)

type ActorDependencies[T plugin.Syncable] struct {
	node           gen.Node
	adapter        *plugin.PluginAdapter[T]
	queueSize      int
	drainTimeout   time.Duration
	healthInterval time.Duration
	retryMin       time.Duration
	retryMax       time.Duration
}

type Deployment struct {
	plugin.BinaryState
	path       string
	hash       string
	rolloutPct float64
}

type DeploymentPoolKey struct {
	pools.PoolKey
	maxProcs int
}

func (d *Deployment) poolKey() DeploymentPoolKey {
	return DeploymentPoolKey{
		PoolKey:  pools.PoolKey{Id: d.Id, Name: d.Name, Hash: d.hash},
		maxProcs: d.workerCount(),
	}
}

func (d *Deployment) workerCount() int {
	return max(1, d.MaxProcs)
}

type Drain struct{}
type Stop struct{}

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

// Cross-component invocation and lifecycle messages.
type InvokeCall[T plugin.Syncable] struct {
	callID     uint64
	context    context.Context
	cancel     context.CancelFunc
	pluginID   string
	rolloutKey string
	fn         func(context.Context, T) error
	shadow     bool
}

type CancelCall struct {
	callID uint64
	err    error
}

type CallCompleted struct {
	callID uint64
	err    error
}

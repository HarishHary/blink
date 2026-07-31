package plugin

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
	ErrRuntimeNotStarted = errors.New("actor runtime not started")
	ErrRuntimeStopped    = errors.New("actor runtime stopped")
	ErrArtifactMismatch  = errors.New("plugin artifact checksum mismatch")
)

var (
	errWorkerRecycle   = errors.New("worker recycled after plugin transport failure")
	errWorkerUnhealthy = errors.New("worker plugin health check failed")
)

type actorDependencies[T plugin.Syncable] struct {
	node           gen.Node
	adapter        *plugin.PluginAdapter[T]
	queueSize      int
	drainTimeout   time.Duration
	healthInterval time.Duration
	retryMin       time.Duration
	retryMax       time.Duration
}

type deployment struct {
	plugin.BinaryState
	path       string
	hash       string
	rolloutPct float64
}

type deploymentPoolKey struct {
	pools.PoolKey
	maxProcs int
}

func (d *deployment) poolKey() deploymentPoolKey {
	return deploymentPoolKey{
		PoolKey:  pools.PoolKey{Id: d.Id, Name: d.Name, Hash: d.hash},
		maxProcs: d.workerCount(),
	}
}

func (d *deployment) workerCount() int {
	return max(1, d.MaxProcs)
}

type retryState struct {
	attempt int
	pending bool
	token   uint64
}

type workerRetryState struct {
	attempt int
	pending bool
	token   uint64
}

func retryDelay(attempt int, minDelay, maxDelay time.Duration) time.Duration {
	shift := attempt
	if shift > 5 {
		shift = 5
	}
	delay := minDelay * time.Duration(1<<shift)
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

type drain struct{}
type stop struct{}

type scheduledBackoff struct {
	strategy *backoff.ExponentialBackOff
	pending  bool
	token    uint64
	cancel   gen.CancelFunc
}

func newScheduledBackoff(minDelay, maxDelay time.Duration) *scheduledBackoff {
	return &scheduledBackoff{
		strategy: backoff.NewExponentialBackOff(
			backoff.WithInitialInterval(minDelay),
			backoff.WithMaxInterval(maxDelay),
			backoff.WithMultiplier(2),
			backoff.WithMaxElapsedTime(0),
		),
	}
}

func (s *scheduledBackoff) cancelScheduled(reset bool) {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.pending = false
	s.token++
	if reset {
		s.strategy.Reset()
	}
}

// Cross-component invocation and lifecycle messages.
type invokeCall[T plugin.Syncable] struct {
	callID     uint64
	context    context.Context
	cancel     context.CancelFunc
	pluginID   string
	rolloutKey string
	fn         func(context.Context, T) error
	shadow     bool
}

type cancelCall struct {
	callID uint64
	err    error
}

type callCompleted struct {
	callID uint64
	err    error
}

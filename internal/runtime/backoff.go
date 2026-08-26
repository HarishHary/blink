package runtime

import (
	"errors"
	"time"

	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
)

var (
	ErrPluginUnavailable = errors.New("plugin unavailable")
	ErrQueueFull         = errors.New("plugin queue full")
	ErrArtifactMismatch  = errors.New("plugin artifact checksum mismatch")
	ErrArtifactScan      = errors.New("plugin artifact scan failed")
	ErrArtifactWatch     = errors.New("plugin artifact watch failed")
	ErrArtifactResolve   = errors.New("plugin artifact resolution failed")
	ErrSnapshotLoad      = errors.New("snapshot state load failed")
	ErrSnapshotWrite     = errors.New("snapshot write failed")
	ErrSnapshotRead      = errors.New("snapshot read failed")
	ErrSnapshotSubscribe = errors.New("snapshot event subscription failed")
	ErrBackoffStopped    = errors.New("scheduled backoff stopped")
	ErrRuntimeNotStarted = errors.New("actor runtime not started")
	ErrRuntimeStopped    = errors.New("actor runtime stopped")
	ErrProcessRecycle    = errors.New("plugin process recycled after transport failure")
	ErrProcessUnhealthy  = errors.New("plugin process health check failed")
)

type ScheduledBackoff struct {
	Strategy backoff.BackOff
	Pending  bool
	Token    uint64
	Cancel   gen.CancelFunc
}

const scheduledRestartLimit uint64 = 5

func NewScheduledBackoff(minDelay, maxDelay time.Duration) *ScheduledBackoff {
	return &ScheduledBackoff{
		Strategy: backoff.WithMaxRetries(backoff.NewExponentialBackOff(
			backoff.WithInitialInterval(minDelay),
			backoff.WithMaxInterval(maxDelay),
			backoff.WithMultiplier(2),
			backoff.WithMaxElapsedTime(0),
		), scheduledRestartLimit),
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

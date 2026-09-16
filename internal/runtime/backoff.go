package runtime

import (
	"errors"
	"time"

	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
)

var ErrBackoffStopped = errors.New("scheduled backoff stopped")

// ScheduledBackoff tracks a bounded retry or replacement schedule and its pending timer.
type ScheduledBackoff struct {
	Strategy backoff.BackOff
	Pending  bool
	Token    uint64
	Cancel   gen.CancelFunc
}

const scheduledRetryLimit uint64 = 5

// NewScheduledBackoff allows five scheduled retries or replacements after the initial attempt.
func NewScheduledBackoff(minDelay, maxDelay time.Duration) *ScheduledBackoff {
	return &ScheduledBackoff{
		Strategy: backoff.WithMaxRetries(backoff.NewExponentialBackOff(
			backoff.WithInitialInterval(minDelay),
			backoff.WithMaxInterval(maxDelay),
			backoff.WithMultiplier(2),
			backoff.WithMaxElapsedTime(0),
		), scheduledRetryLimit),
	}
}

// CancelScheduled drops any pending retry and invalidates its token, resetting the progression on reset.
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

package snapshot

import (
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

const (
	snapshotConfigRetryMin = 100 * time.Millisecond
	snapshotConfigRetryMax = 5 * time.Second
)

// MessageSnapshotConfigSubscribe is handled after process initialization because
// MonitorEvent is a running-state operation.
type MessageSnapshotConfigSubscribe struct{ token uint64 }

type snapshotConfigSubscriptionState struct {
	snapshot bool
	status   bool
	ready    bool
	retry    *runtime.ScheduledBackoff
}

type snapshotConfigActor[T plugin.Syncable] struct {
	act.Actor
	cache        *config.SnapshotConfig[T]
	events       Events
	subscription snapshotConfigSubscriptionState
}

// NewConfigActor creates a subscriber that projects buffered snapshot and
// status events into a SnapshotConfig cache used by the existing data-plane
// code. It owns no snapshot-reader lifecycle.
func NewConfigActor[T plugin.Syncable](cache *config.SnapshotConfig[T], events Events) gen.ProcessBehavior {
	return &snapshotConfigActor[T]{cache: cache, events: events}
}

func (a *snapshotConfigActor[T]) Init(...any) error {
	if a.cache == nil {
		return fmt.Errorf("snapshot config actor: cache is required")
	}
	a.subscription.retry = runtime.NewScheduledBackoff(snapshotConfigRetryMin, snapshotConfigRetryMax)
	return a.Send(a.PID(), MessageSnapshotConfigSubscribe{})
}

func (a *snapshotConfigActor[T]) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case MessageSnapshotConfigSubscribe:
		if m.token != 0 {
			if !a.subscription.retry.Pending || m.token != a.subscription.retry.Token {
				return nil
			}
			a.subscription.retry.Pending = false
			a.subscription.retry.Cancel = nil
		}
		if err := a.subscribe(); err != nil {
			a.updateReady()
			return a.scheduleSubscribe()
		}
		a.subscription.retry.CancelScheduled(true)

	case gen.MessageDownEvent:
		switch m.Event {
		case a.events.Snapshot:
			a.subscription.snapshot = false
		case a.events.Status:
			a.subscription.status = false
			a.subscription.ready = false
		default:
			return nil
		}
		a.updateReady()
		a.subscription.retry.CancelScheduled(true)
		return a.Send(a.PID(), MessageSnapshotConfigSubscribe{})
	}
	return nil
}

func (a *snapshotConfigActor[T]) HandleEvent(event gen.MessageEvent) error {
	a.applyEvent(event)
	return nil
}

func (a *snapshotConfigActor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("snapshot config actor: unsupported call %T", request)
}

func (a *snapshotConfigActor[T]) Terminate(error) {
	a.subscription.retry.CancelScheduled(false)
	a.subscription.snapshot = false
	a.subscription.status = false
	a.subscription.ready = false
	a.updateReady()
}

func (a *snapshotConfigActor[T]) applyEvent(event gen.MessageEvent) {
	switch event.Event {
	case a.events.Snapshot:
		if !a.subscription.snapshot {
			return
		}
		snap, ok := event.Message.(*snapshot.Snapshot)
		if ok && snap != nil {
			a.cache.Apply(snap)
		}

	case a.events.Status:
		if !a.subscription.status {
			return
		}
		status, ok := event.Message.(SnapshotReaderStatus)
		if ok {
			a.subscription.ready = status.Availability == runtime.AvailabilityReady
			a.updateReady()
		}
	}
}

func (a *snapshotConfigActor[T]) subscribe() error {
	if !a.subscription.snapshot {
		buffered, err := a.MonitorEvent(a.events.Snapshot)
		if err != nil {
			return fmt.Errorf("%w: monitor snapshot event: %w", runtime.ErrSnapshotSubscribe, err)
		}
		a.subscription.snapshot = true
		for _, event := range buffered {
			a.applyEvent(event)
		}
	}

	if !a.subscription.status {
		buffered, err := a.MonitorEvent(a.events.Status)
		if err != nil {
			return fmt.Errorf("%w: monitor snapshot status event: %w", runtime.ErrSnapshotSubscribe, err)
		}
		a.subscription.status = true
		for _, event := range buffered {
			a.applyEvent(event)
		}
	}
	a.updateReady()
	return nil
}

func (a *snapshotConfigActor[T]) scheduleSubscribe() error {
	if a.subscription.retry.Pending {
		return nil
	}
	delay := a.subscription.retry.Strategy.NextBackOff()
	if delay == backoff.Stop {
		return fmt.Errorf("%w: subscription retry backoff stopped", runtime.ErrSnapshotSubscribe)
	}
	a.subscription.retry.Token++
	token := a.subscription.retry.Token
	cancel, err := a.SendAfter(a.PID(), MessageSnapshotConfigSubscribe{token: token}, delay)
	if err != nil {
		return fmt.Errorf("%w: schedule subscription retry: %w", runtime.ErrSnapshotSubscribe, err)
	}
	a.subscription.retry.Pending = true
	a.subscription.retry.Cancel = cancel
	return nil
}

func (a *snapshotConfigActor[T]) updateReady() {
	if a.cache != nil {
		a.cache.SetReaderReady(
			a.subscription.snapshot && a.subscription.status && a.subscription.ready,
		)
	}
}

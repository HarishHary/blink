package snapshot

import (
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// snapshotConfigSubscribe is handled after process initialization because
// MonitorEvent is a running-state operation.
type snapshotConfigSubscribe struct{}

type snapshotConfigActor[T plugin.Syncable] struct {
	act.Actor
	cache  *config.SnapshotConfig[T]
	events Events
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
	return a.Send(a.PID(), snapshotConfigSubscribe{})
}

func (a *snapshotConfigActor[T]) HandleMessage(_ gen.PID, message any) error {
	switch m := message.(type) {
	case snapshotConfigSubscribe:
		bufferedSnapshots, err := a.MonitorEvent(a.events.Snapshot)
		if err != nil {
			return fmt.Errorf("monitor snapshot event: %w", err)
		}
		bufferedStatuses, err := a.MonitorEvent(a.events.Status)
		if err != nil {
			return fmt.Errorf("monitor snapshot status event: %w", err)
		}
		for _, event := range bufferedSnapshots {
			a.applyEvent(event)
		}
		for _, event := range bufferedStatuses {
			a.applyEvent(event)
		}

	case gen.MessageDownEvent:
		if m.Event == a.events.Snapshot || m.Event == a.events.Status {
			a.cache.SetReaderReady(false)
		}
	}
	return nil
}

func (a *snapshotConfigActor[T]) HandleEvent(event gen.MessageEvent) error {
	a.applyEvent(event)
	return nil
}

func (a *snapshotConfigActor[T]) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (a *snapshotConfigActor[T]) Terminate(error) {
	a.cache.SetReaderReady(false)
}

func (a *snapshotConfigActor[T]) applyEvent(event gen.MessageEvent) {
	switch event.Event {
	case a.events.Snapshot:
		snap, ok := event.Message.(*snapshot.Snapshot)
		if ok && snap != nil {
			a.cache.Apply(snap)
		}

	case a.events.Status:
		status, ok := event.Message.(SnapshotReaderStatus)
		if ok {
			a.cache.SetReaderReady(status.Availability == runtime.AvailabilityReady)
		}
	}
}

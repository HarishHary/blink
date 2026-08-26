package snapshot

import (
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// NetworkTypes registers the wire vocabulary defined in snapshot_reader_actor.go - the messages a
// controller actor and a subscribing executor's readerActor exchange over the Ergo cluster.
//
// One list shared by both sides' Load (via gen.ApplicationSpec.Network.RegisterTypes) so
// registration can't drift between them. Values, not pointers, per RegisterType's contract, and
// safe to re-register an already-covered nested type (gen.ErrTaken).
func NetworkTypes() []any {
	return []any{
		SubscribeRequest{},
		SubscribeResponse{},
		SnapshotUpdate{},
		UnsubscribeRequest{},
		MessageExecutorReport{},
		ExecutorHeartbeat{},
		ExecutorAppliedGeneration{},
		snapshot.EntryChange{},
		snapshot.ChangeKind(0),
		snapshot.Snapshot{},
		snapshot.EffectiveEntry{},
		snapshot.ArtifactRef{},
		runtime.RolloutMode(0),
	}
}

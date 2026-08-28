package snapshot

import (
	"ergo.services/ergo/gen"

	"github.com/harishhary/blink/internal/runtime"
)

// ControllerActorName is the registered name a namespace's controller actor answers on, the address
// this vocabulary is spoken to.
func ControllerActorName(namespace string) gen.Atom {
	return gen.Atom("controller-" + namespace + "-actor")
}

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
		EntryChange{},
		ChangeKind(0),
		Snapshot{},
		EffectiveEntry{},
		ArtifactRef{},
		runtime.RolloutMode(0),
	}
}

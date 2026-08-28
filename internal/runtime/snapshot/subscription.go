package snapshot

import (
	"ergo.services/ergo/gen"

	"github.com/harishhary/blink/internal/runtime"
)

// ControllerActorName is the registered name a namespace's controller actor answers on.
func ControllerActorName(namespace string) gen.Atom {
	return gen.Atom("controller-" + namespace + "-actor")
}

// NetworkTypes is the wire vocabulary a controller actor and a subscribing readerActor exchange,
// one list shared by both sides' Load so registration cannot drift; values, not pointers.
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

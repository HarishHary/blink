package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
)

// SupervisorOptions configures a raw reader and its typed projection sibling. Namespace is the
// subtree's own: it names every process here, labels its series, and is the controller namespace the
// reader follows. It never crosses the cluster. Name overrides the registered supervisor name only;
// both children's names are derived from the namespace.
type SupervisorOptions[T any] struct {
	Name               gen.Atom
	Namespace          string
	Loader             Loader[T]
	ProjectionMode     ProjectionCommitMode
	Stopped            chan<- error
	ReaderActorOptions ReaderActorOptions
}

// ReaderActorOptions configures the reader child of a snapshot supervisor: which controller actor to
// subscribe to over the Ergo cluster, and how this executor identifies itself to it.
type ReaderActorOptions struct {
	Endpoint   gen.ProcessID
	ExecutorID string
	RestartMin time.Duration
	RestartMax time.Duration
}

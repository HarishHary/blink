package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
)

// SupervisorOptions configures a raw reader and its typed projection sibling.
type SupervisorOptions[T any] struct {
	Name               gen.Atom
	Loader             Loader[T]
	ProjectionMode     ProjectionCommitMode
	Stopped            chan<- error
	ReaderActorOptions ReaderActorOptions
}

// ReaderActorOptions configures the reader child of a snapshot supervisor: which namespace
// controller actor to subscribe to over the Ergo cluster, and how this executor identifies itself.
// Namespace never crosses the cluster; it names every process in the subtree and labels its series.
type ReaderActorOptions struct {
	Name       gen.Atom
	Namespace  string
	Endpoint   gen.ProcessID
	ExecutorID string
	RestartMin time.Duration
	RestartMax time.Duration
}

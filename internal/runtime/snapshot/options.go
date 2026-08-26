package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
)

// ReaderActorOptions configures the reader child of a snapshot supervisor: which namespace
// controller actor to subscribe to over the Ergo cluster, and how this executor identifies itself.
type ReaderActorOptions struct {
	Name       gen.Atom
	Endpoint   gen.ProcessID
	ExecutorID string
	Role       string
	RestartMin time.Duration
	RestartMax time.Duration
}

// SupervisorOptions configures a raw reader and its typed projection sibling.
type SupervisorOptions[T any] struct {
	ReaderActorOptions ReaderActorOptions
	Loader             Loader[T]
	ProjectionMode     ProjectionCommitMode
	Stopped            chan<- error
}

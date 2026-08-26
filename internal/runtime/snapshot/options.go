package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
)

// ReaderActorOptions configures the reader child of a snapshot supervisor.
type ReaderActorOptions struct {
	Name          gen.Atom
	ReaderFactory func() brokers.Reader
	RestartMin    time.Duration
	RestartMax    time.Duration
}

// SupervisorOptions configures a raw reader and its typed projection sibling.
type SupervisorOptions[T any] struct {
	ReaderActorOptions
	Loader         Loader[T]
	ProjectionMode ProjectionCommitMode
	Stopped        chan<- error
}

package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
)

// SnapshotReaderActorOptions configures the reader child of a snapshot supervisor.
type SnapshotReaderActorOptions struct {
	Name          gen.Atom
	Logger        *logger.Logger
	ReaderFactory func() brokers.Reader
	RestartMin    time.Duration
	RestartMax    time.Duration
}

// SnapshotReaderSupervisorOptions configures a raw reader and its typed projection sibling.
type SnapshotReaderSupervisorOptions[T any] struct {
	SnapshotReaderActorOptions
	Projection     ProjectionSpec[T]
	ProjectionMode ProjectionCommitMode
	Stopped        chan<- error
}

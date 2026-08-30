package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
)

// SupervisorOptions configures a raw reader and its typed projection sibling; Namespace names every
// process here, labels its series, and is the controller namespace the reader follows.
type SupervisorOptions struct {
	Namespace          string
	ProjectionMode     ProjectionCommitMode
	Stopped            chan<- error
	ReaderActorOptions ReaderActorOptions
}

// ReaderActorOptions configures the reader child: the controller actor it subscribes to and the ID
// it subscribes as.
type ReaderActorOptions struct {
	Endpoint   gen.ProcessID
	ExecutorID string
	RestartMin time.Duration
	RestartMax time.Duration
}

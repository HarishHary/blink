package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
)

const (
	DefaultRetryMin = 100 * time.Millisecond
	DefaultRetryMax = 5 * time.Second
)

// Every process in a subtree is named from its namespace, mirroring controller-<namespace>-*.
func SupervisorName(namespace string) gen.Atom {
	return subtreeName(namespace, "supervisor")
}

// ReaderActorName is the reader child's registered name.
func ReaderActorName(namespace string) gen.Atom {
	return subtreeName(namespace, "reader-actor")
}

// ProjectionActorName is the projection child's registered name.
func ProjectionActorName(namespace string) gen.Atom {
	return subtreeName(namespace, "projection-actor")
}

// subtreeName builds one snapshot subtree name from its namespace.
func subtreeName(namespace, suffix string) gen.Atom {
	return gen.Atom("snapshot-" + namespace + "-" + suffix)
}

// supervisorOptionsWithDefaults fills reader supervisor option defaults.
func supervisorOptionsWithDefaults(opts SupervisorOptions) SupervisorOptions {
	opts.ReaderActorOptions = readerOptionsWithDefaults(opts.ReaderActorOptions)
	return opts
}

// readerOptionsWithDefaults fills reader actor option defaults.
func readerOptionsWithDefaults(opts ReaderActorOptions) ReaderActorOptions {
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

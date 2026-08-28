package snapshot

import (
	"time"

	"ergo.services/ergo/gen"
)

const (
	DefaultRestartMin = 100 * time.Millisecond
	DefaultRestartMax = 5 * time.Second
)

// Every process in a namespace's subtree is named from that namespace, mirroring the controller's
// controller-<namespace>-* names, so a caller addresses one without being told what it was called.
func SupervisorName(namespace string) gen.Atom {
	return subtreeName(namespace, "supervisor")
}

func ReaderActorName(namespace string) gen.Atom {
	return subtreeName(namespace, "reader-actor")
}

func ProjectionActorName(namespace string) gen.Atom {
	return subtreeName(namespace, "projection-actor")
}

func subtreeName(namespace, suffix string) gen.Atom {
	return gen.Atom("snapshot-" + namespace + "-" + suffix)
}

// supervisorOptionsWithDefaults fills reader supervisor option defaults.
func supervisorOptionsWithDefaults[T any](opts SupervisorOptions[T]) SupervisorOptions[T] {
	if opts.Name == "" {
		opts.Name = SupervisorName(opts.ReaderActorOptions.Namespace)
	}
	opts.ReaderActorOptions = readerOptionsWithDefaults(opts.ReaderActorOptions)
	return opts
}

// readerOptionsWithDefaults fills reader actor option defaults.
func readerOptionsWithDefaults(opts ReaderActorOptions) ReaderActorOptions {
	if opts.Name == "" {
		opts.Name = ReaderActorName(opts.Namespace)
	}
	if opts.RestartMin <= 0 {
		opts.RestartMin = DefaultRestartMin
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = DefaultRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	return opts
}

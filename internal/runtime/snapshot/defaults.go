package snapshot

import "time"

const (
	DefaultRestartMin = 100 * time.Millisecond
	DefaultRestartMax = 5 * time.Second
)

// snapshotReaderSupervisorOptionsWithDefaults fills reader supervisor option defaults.
func snapshotReaderSupervisorOptionsWithDefaults[T any](opts SnapshotReaderSupervisorOptions[T]) SnapshotReaderSupervisorOptions[T] {
	opts.SnapshotReaderActorOptions = snapshotReaderActorOptionsWithDefaults(opts.SnapshotReaderActorOptions)
	return opts
}

// snapshotReaderActorOptionsWithDefaults fills reader actor option defaults.
func snapshotReaderActorOptionsWithDefaults(opts SnapshotReaderActorOptions) SnapshotReaderActorOptions {
	if opts.Name == "" {
		opts.Name = "snapshot-reader"
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

package snapshot

import "time"

const (
	DefaultRestartMin = 100 * time.Millisecond
	DefaultRestartMax = 5 * time.Second
)

// optionsWithDefaults fills reader supervisor option defaults.
func optionsWithDefaults[T any](opts SupervisorOptions[T]) SupervisorOptions[T] {
	opts.ReaderActorOptions = readerOptionsWithDefaults(opts.ReaderActorOptions)
	return opts
}

// readerOptionsWithDefaults fills reader actor option defaults.
func readerOptionsWithDefaults(opts ReaderActorOptions) ReaderActorOptions {
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

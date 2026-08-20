package controller

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

const (
	DefaultRestartMin = 100 * time.Millisecond
	DefaultRestartMax = 5 * time.Second
)

// optionsWithDefaults fills application option defaults.
func optionsWithDefaults[T plugin.Artifact](opts Options[T]) Options[T] {
	if opts.Name == "" {
		opts.Name = gen.Atom("controller-" + opts.Namespace)
	}
	opts.SupervisorOptions = supervisorOptionsWithDefaults(opts.Name, opts.SupervisorOptions)
	return opts
}

// supervisorOptionsWithDefaults fills supervisor option defaults.
func supervisorOptionsWithDefaults[T plugin.Artifact](applicationName gen.Atom, opts SupervisorOptions[T]) SupervisorOptions[T] {
	if opts.Name == "" {
		opts.Name = applicationName + "-supervisor"
	}
	opts.ActorOptions = actorOptionsWithDefaults(applicationName, opts.ActorOptions)
	return opts
}

// actorOptionsWithDefaults fills actor names and timing defaults.
func actorOptionsWithDefaults[T plugin.Artifact](applicationName gen.Atom, opts ActorOptions[T]) ActorOptions[T] {
	if opts.Name == "" {
		opts.Name = applicationName + "-actor"
	}
	if opts.RestartMin <= 0 {
		opts.RestartMin = DefaultRestartMin
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = DefaultRestartMax
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = opts.RestartMin
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RestartMax
	}
	return opts
}

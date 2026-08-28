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

// applicationOptionsWithDefaults fills application option defaults.
func applicationOptionsWithDefaults[T plugin.Artifact](opts ApplicationOptions[T]) ApplicationOptions[T] {
	name := gen.Atom("controller-" + opts.Namespace)
	if opts.Name == "" {
		opts.Name = name + "-application"
	}
	if opts.SupervisorOptions.Namespace == "" {
		opts.SupervisorOptions.Namespace = opts.Namespace
	}
	opts.SupervisorOptions = supervisorOptionsWithDefaults(name, opts.SupervisorOptions)
	return opts
}

// supervisorOptionsWithDefaults fills supervisor option defaults.
func supervisorOptionsWithDefaults[T plugin.Artifact](supervisorName gen.Atom, opts SupervisorOptions[T]) SupervisorOptions[T] {
	if opts.Name == "" {
		opts.Name = supervisorName + "-supervisor"
	}
	if opts.ActorOptions.Namespace == "" {
		opts.ActorOptions.Namespace = opts.Namespace
	}
	opts.ActorOptions = actorOptionsWithDefaults(supervisorName, opts.ActorOptions)
	return opts
}

// actorOptionsWithDefaults fills actor names and timing defaults.
func actorOptionsWithDefaults[T plugin.Artifact](actorName gen.Atom, opts ActorOptions[T]) ActorOptions[T] {
	if opts.Name == "" {
		opts.Name = actorName + "-actor"
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

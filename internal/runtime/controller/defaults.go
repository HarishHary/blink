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

// controllerApplicationOptionsWithDefaults fills application option defaults.
func controllerApplicationOptionsWithDefaults[T plugin.Syncable](opts ControllerApplicationOptions[T]) ControllerApplicationOptions[T] {
	if opts.Name == "" {
		opts.Name = gen.Atom("controller-" + opts.Namespace)
	}
	opts.SupervisorOptions = controllerSupervisorOptionsWithDefaults(opts.Name, opts.SupervisorOptions)
	return opts
}

// controllerSupervisorOptionsWithDefaults fills supervisor option defaults.
func controllerSupervisorOptionsWithDefaults[T plugin.Syncable](applicationName gen.Atom, opts ControllerSupervisorOptions[T]) ControllerSupervisorOptions[T] {
	if opts.Name == "" {
		opts.Name = applicationName + "-supervisor"
	}
	opts.ActorOptions = controllerActorOptionsWithDefaults(applicationName, opts.ActorOptions)
	return opts
}

// controllerActorOptionsWithDefaults fills actor names and timing defaults.
func controllerActorOptionsWithDefaults[T plugin.Syncable](applicationName gen.Atom, opts ControllerActorOptions[T]) ControllerActorOptions[T] {
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

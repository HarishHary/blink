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

func controllerApplicationOptionsWithDefaults[T plugin.Syncable](opts ControllerApplicationOptions[T]) ControllerApplicationOptions[T] {
	if opts.Name == "" {
		opts.Name = gen.Atom("controller-" + opts.Namespace)
	}
	if opts.SupervisorName == "" {
		opts.SupervisorName = opts.Name + "-supervisor"
	}
	if opts.ActorName == "" {
		opts.ActorName = opts.Name + "-actor"
	}
	opts.Actor = controllerActorOptionsWithDefaults(opts.Actor)
	return opts
}

func controllerActorOptionsWithDefaults[T plugin.Syncable](opts ControllerActorOptions[T]) ControllerActorOptions[T] {
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

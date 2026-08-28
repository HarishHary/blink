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

// Every process in a namespace's subtree is named from that namespace, so a caller addresses one
// without being told what it was called.
func ApplicationName(namespace string) gen.Atom { return subtreeName(namespace, "application") }
func SupervisorName(namespace string) gen.Atom  { return subtreeName(namespace, "supervisor") }
func ActorName(namespace string) gen.Atom       { return subtreeName(namespace, "actor") }

func subtreeName(namespace, suffix string) gen.Atom {
	return gen.Atom("controller-" + namespace + "-" + suffix)
}

// applicationOptionsWithDefaults fills application option defaults.
func applicationOptionsWithDefaults[T plugin.Artifact](opts ApplicationOptions[T]) ApplicationOptions[T] {
	if opts.Name == "" {
		opts.Name = ApplicationName(opts.Namespace)
	}
	if opts.SupervisorOptions.Namespace == "" {
		opts.SupervisorOptions.Namespace = opts.Namespace
	}
	opts.SupervisorOptions = supervisorOptionsWithDefaults(opts.SupervisorOptions)
	return opts
}

// supervisorOptionsWithDefaults fills supervisor option defaults.
func supervisorOptionsWithDefaults[T plugin.Artifact](opts SupervisorOptions[T]) SupervisorOptions[T] {
	if opts.Name == "" {
		opts.Name = SupervisorName(opts.Namespace)
	}
	if opts.ActorOptions.Namespace == "" {
		opts.ActorOptions.Namespace = opts.Namespace
	}
	opts.ActorOptions = actorOptionsWithDefaults(opts.ActorOptions)
	return opts
}

// actorOptionsWithDefaults fills actor names and timing defaults.
func actorOptionsWithDefaults[T plugin.Artifact](opts ActorOptions[T]) ActorOptions[T] {
	if opts.Name == "" {
		opts.Name = ActorName(opts.Namespace)
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

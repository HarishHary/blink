package controller

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

const (
	DefaultRestartMin = 100 * time.Millisecond
	DefaultRestartMax = 5 * time.Second
)

// Every process in a namespace's subtree is named from that namespace, so a caller addresses one
// without being told what it was called.
func ApplicationName(namespace string) gen.Atom { return subtreeName(namespace, "application") }
func SupervisorName(namespace string) gen.Atom  { return subtreeName(namespace, "supervisor") }

// ActorName is the one name that crosses the cluster - executors subscribe to it - so it is defined
// with the wire vocabulary they share and only re-exported here.
func ActorName(namespace string) gen.Atom { return snapshot.ControllerActorName(namespace) }

func subtreeName(namespace, suffix string) gen.Atom {
	return gen.Atom("controller-" + namespace + "-" + suffix)
}

// applicationOptionsWithDefaults fills application option defaults.
func applicationOptionsWithDefaults(opts ApplicationOptions) ApplicationOptions {
	opts.SupervisorOptions = supervisorOptionsWithDefaults(opts.SupervisorOptions)
	return opts
}

// supervisorOptionsWithDefaults fills supervisor option defaults.
func supervisorOptionsWithDefaults(opts SupervisorOptions) SupervisorOptions {
	opts.ActorOptions = actorOptionsWithDefaults(opts.ActorOptions)
	return opts
}

// actorOptionsWithDefaults fills actor timing defaults.
func actorOptionsWithDefaults(opts ActorOptions) ActorOptions {
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

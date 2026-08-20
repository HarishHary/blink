package controller

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

// Options configures one plugin-type controller application.
type Options[T plugin.Artifact] struct {
	// Names default to controller-<namespace>, <name>-supervisor, and <name>-actor.
	Name              gen.Atom
	DatabaseDSN       string
	Namespace         string
	Topic             string
	Broker            brokers.Broker
	SupervisorOptions SupervisorOptions[T]
}

type SupervisorOptions[T plugin.Artifact] struct {
	Name         gen.Atom
	ActorOptions ActorOptions[T]
}

// ActorOptions configures the controller actor.
type ActorOptions[T plugin.Artifact] struct {
	Name       gen.Atom
	Directory  string
	Loader     plugin.Loader[T]
	RestartMin time.Duration
	RestartMax time.Duration
	RetryMin   time.Duration
	RetryMax   time.Duration
}

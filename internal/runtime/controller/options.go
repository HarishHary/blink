package controller

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

// ControllerApplicationOptions configures one plugin-type controller application.
type ControllerApplicationOptions[T plugin.Syncable] struct {
	// Names default to controller-<namespace>, <name>-supervisor, and <name>-actor.
	Name              gen.Atom
	DatabaseDSN       string
	Namespace         string
	Topic             string
	Broker            brokers.Broker
	SupervisorOptions ControllerSupervisorOptions[T]
}

type ControllerSupervisorOptions[T plugin.Syncable] struct {
	Name         gen.Atom
	ActorOptions ControllerActorOptions[T]
}

// ControllerActorOptions configures the controller actor.
type ControllerActorOptions[T plugin.Syncable] struct {
	Name       gen.Atom
	Directory  string
	Loader     plugin.Loader[T]
	RestartMin time.Duration
	RestartMax time.Duration
	RetryMin   time.Duration
	RetryMax   time.Duration
}

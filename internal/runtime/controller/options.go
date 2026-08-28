package controller

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

// ApplicationOptions configures one plugin-type controller application.
type ApplicationOptions[T plugin.Artifact] struct {
	Name              gen.Atom
	DatabaseDSN       string
	Namespace         string
	SupervisorOptions SupervisorOptions[T]
}

// SupervisorOptions configures one plugin-type controller supervisor.
type SupervisorOptions[T plugin.Artifact] struct {
	Name         gen.Atom
	Namespace    string
	ActorOptions ActorOptions[T]
}

// ActorOptions configures one plugin-type controller actor.
type ActorOptions[T plugin.Artifact] struct {
	Name       gen.Atom
	Namespace  string
	Directory  string
	Loader     plugin.Loader[T]
	RestartMin time.Duration
	RestartMax time.Duration
	RetryMin   time.Duration
	RetryMax   time.Duration
}

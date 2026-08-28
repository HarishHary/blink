package controller

import "time"

// ApplicationOptions configures one plugin-type controller application. Namespace is the only name it
// needs; the typed loader is a dependency NewService and NewApplication take directly.
type ApplicationOptions struct {
	Namespace         string
	DatabaseDSN       string
	SupervisorOptions SupervisorOptions
}

// SupervisorOptions configures one plugin-type controller supervisor. It carries no namespace: the
// application configures the only one and hands it to the supervisor.
type SupervisorOptions struct {
	ActorOptions ActorOptions
}

// ActorOptions configures one plugin-type controller actor. As a supervisor child it carries neither
// name nor namespace; its supervisor hands it the namespace's labels and the loader.
type ActorOptions struct {
	Directory  string
	RestartMin time.Duration
	RestartMax time.Duration
	RetryMin   time.Duration
	RetryMax   time.Duration
}

package controller

import "time"

// ApplicationOptions configures one plugin-type controller application. Namespace is the only name
// it needs: ApplicationName, SupervisorName, and ActorName all derive from it. The typed loader is a
// dependency, not an option: NewService and NewApplication take it directly.
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

// ActorOptions configures one plugin-type controller actor. The actor is a supervisor child, so it
// carries neither name nor namespace: ActorName derives the one executors subscribe to, and its
// supervisor hands it the labels the namespace produced, along with the loader it parses through.
type ActorOptions struct {
	Directory  string
	RestartMin time.Duration
	RestartMax time.Duration
	RetryMin   time.Duration
	RetryMax   time.Duration
}

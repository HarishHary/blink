package plugin

import (
	"time"

	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// ApplicationOptions configures one plugin actor subtree on a process-owned Ergo node. Namespace is
// configured here alone, and names the whole subtree and every series from it.
type ApplicationOptions struct {
	Namespace          string
	MaxBatchSize       int
	MaxConcurrentCalls int
	CloseTimeout       time.Duration
	// How wide one call may fan out, derived from the two knobs above in runtimeOptionsWithDefaults.
	// The gateway's budgets derive from it there too: a budget set apart from the fan-out it has to
	// hold is a budget that rejects a legitimate call.
	callFanOut        int
	GatewayOptions    GatewayOptions
	SupervisorOptions SupervisorOptions
}

// GatewayOptions configures one invocation gateway. Every budget defaults from the application's two
// knobs above, so a caller never has to size them.
type GatewayOptions struct {
	MaxOutstandingInvocations          int
	MaxOutstandingInvocationsPerPlugin int
	ShadowMaxOutstandingInvocations    int
	MaxWaiting                         int           // callers that may wait for a production permit at once
	SubmitTimeout                      time.Duration // bounds a submission from a caller that set no deadline
}

// SupervisorOptions configures a runtime supervisor. It carries no name and no namespace: the
// application configures the namespace, and every name in the subtree derives from it.
type SupervisorOptions struct {
	Directory      string
	RetryMin       time.Duration
	RetryMax       time.Duration
	RestartMin     time.Duration
	RestartMax     time.Duration
	ControlTimeout time.Duration
	CatalogOptions CatalogOptions
	SnapshotReader snapshot.ReaderActorOptions
}

// CatalogOptions configures one plugin catalog and the routers it spawns.
type CatalogOptions struct {
	RestartMin    time.Duration
	RestartMax    time.Duration
	RouterOptions RouterOptions // handed straight to each spawned router
}

// RouterOptions configures one deployment router and its route lifecycle-step retries.
type RouterOptions struct {
	RetryMin                 time.Duration
	RetryMax                 time.Duration
	DeploymentManagerOptions DeploymentManagerOptions // handed straight to each spawned manager
}

// DeploymentManagerOptions configures one deployment manager. Restart bounds pace replacing a lost process;
// exhaustion opens its circuit. ProcessBudget is shared by every manager
// in the process and bounds their combined scale-up past min_procs, nil leaving each to its max_procs.
type DeploymentManagerOptions struct {
	QueueSize            int
	DispatchTimeout      time.Duration
	ScaleCooldown        time.Duration
	IdleTimeout          time.Duration
	DrainTimeout         time.Duration
	CircuitCooldown      time.Duration // how long an open circuit waits before admitting calls again
	RestartMin           time.Duration
	RestartMax           time.Duration
	ProcessBudget        *ProcessBudget
	PluginProcessOptions PluginProcessOptions // handed to each spawned plugin process
}

// PluginProcessOptions configures one plugin process.
type PluginProcessOptions struct {
	InvocationTimeout time.Duration
	HealthInterval    time.Duration
	RestartMin        time.Duration
	RestartMax        time.Duration
}

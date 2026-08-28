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
	// Admission budgets, derived from the two knobs above in runtimeOptionsWithDefaults: a budget
	// set apart from the fan-out it has to hold is a budget that rejects a legitimate call.
	callFanOut                         int
	maxOutstandingInvocations          int
	maxOutstandingInvocationsPerPlugin int
	shadowMaxOutstandingInvocations    int
	SupervisorOptions                  SupervisorOptions
}

// SupervisorOptions configures a runtime supervisor. It carries no name and no namespace: the
// application configures the namespace, and every name in the subtree derives from it.
type SupervisorOptions struct {
	Directory      string
	RetryMin       time.Duration
	RetryMax       time.Duration
	ControlTimeout time.Duration
	CatalogOptions CatalogOptions
	SnapshotReader snapshot.ReaderActorOptions
}

// CatalogOptions configures one plugin catalog and the routers it spawns.
type CatalogOptions struct {
	RetryMin      time.Duration // router-level restart backoff
	RetryMax      time.Duration
	RouterOptions RouterOptions // handed straight to each spawned router
}

// RouterOptions configures one deployment router and the managers it spawns.
type RouterOptions struct {
	RetryMin                 time.Duration // route-level restart backoff
	RetryMax                 time.Duration
	DeploymentManagerOptions DeploymentManagerOptions // handed straight to each spawned manager
}

// DeploymentManagerOptions configures one deployment manager.
type DeploymentManagerOptions struct {
	QueueSize       int
	DispatchTimeout time.Duration
	ScaleCooldown   time.Duration
	IdleTimeout     time.Duration
	DrainTimeout    time.Duration
	CircuitCooldown time.Duration // how long an open circuit waits before admitting calls again
	// RetryMin and RetryMax pace replacing a lost plugin process, and their budget is what the circuit
	// above opens on; process recovery is the manager's own job.
	RetryMin time.Duration
	RetryMax time.Duration
	// ProcessBudget is shared by every manager in the process and bounds their combined
	// scale-up past min_procs; nil leaves each manager bounded only by its own max_procs.
	ProcessBudget        *ProcessBudget
	PluginProcessOptions PluginProcessOptions // handed to each spawned plugin process
}

// PluginProcessOptions configures one plugin process.
type PluginProcessOptions struct {
	InvocationTimeout time.Duration
	HealthInterval    time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
}

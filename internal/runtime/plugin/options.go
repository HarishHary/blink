package plugin

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// Options configures one plugin actor subtree on a process-owned Ergo node.
type Options struct {
	SupervisorOptions SupervisorOptions
	// MaxBatchSize is the largest batch a caller submits in one call, and MaxConcurrentCalls
	// is how many such calls it runs at once. The admission budgets below are sized from the
	// two of them when they are left unset: a batch caps a call's fan-out because a shard needs
	// an event to carry, and MaxDeploymentProcs caps it otherwise, so leaving MaxBatchSize unset
	// sizes them for the widest fan-out any deployment may declare rather than for a guess.
	MaxBatchSize       int
	MaxConcurrentCalls int
	// MaxOutstandingInvocations bounds all accepted production invocations,
	// including calls waiting in deployment queues.
	MaxOutstandingInvocations int
	// MaxOutstandingInvocationsPerPlugin bounds the share of that budget one
	// plugin may hold, so a saturated plugin cannot starve the others.
	MaxOutstandingInvocationsPerPlugin int
	// ShadowMaxOutstandingInvocations is an independent best-effort budget.
	ShadowMaxOutstandingInvocations int
	CloseTimeout                    time.Duration
}

// SupervisorOptions configures a runtime supervisor.
type SupervisorOptions struct {
	Name           gen.Atom
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
	RetryMin       time.Duration // route-level restart backoff
	RetryMax       time.Duration
	ManagerOptions DeploymentManagerOptions // handed straight to each spawned manager
}

// DeploymentManagerOptions configures one deployment manager.
type DeploymentManagerOptions struct {
	QueueSize       int
	DispatchTimeout time.Duration
	ScaleCooldown   time.Duration
	IdleTimeout     time.Duration
	DrainTimeout    time.Duration
	CircuitCooldown time.Duration // how long an open circuit waits before admitting calls again
	// RetryMin and RetryMax pace replacing a plugin process the manager lost, and their budget is
	// what the circuit above opens on: process recovery is the manager's own job now that nothing
	// sits between it and the processes it owns.
	RetryMin time.Duration
	RetryMax time.Duration
	// WorkerBudget is shared by every manager in the process and bounds their combined
	// scale-up past min_procs; nil leaves each manager bounded only by its own max_procs.
	WorkerBudget   *WorkerBudget
	ProcessOptions PluginProcessOptions // handed to each spawned plugin process
}

// PluginProcessOptions configures one plugin process.
type PluginProcessOptions struct {
	InvocationTimeout time.Duration
	HealthInterval    time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
}

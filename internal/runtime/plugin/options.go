package plugin

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// Options configures one plugin actor subtree on a process-owned Ergo node.
type Options struct {
	SupervisorOptions SupervisorOptions
	// MaxOutstandingInvocations bounds all accepted production invocations,
	// including calls waiting in deployment queues.
	MaxOutstandingInvocations int
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
	PoolOptions     DeploymentPoolOptions
}

// DeploymentPoolOptions configures one deployment worker pool.
type DeploymentPoolOptions struct {
	InitialSize   int64
	MaxSize       int64
	RetryMin      time.Duration
	RetryMax      time.Duration
	WorkerOptions DeploymentWorkerOptions
}

// DeploymentWorkerOptions configures one plugin execution worker.
type DeploymentWorkerOptions struct {
	InvocationTimeout time.Duration
	HealthInterval    time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
}

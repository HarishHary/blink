package plugin

import (
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// supervisorOptions configures a runtime supervisor.
type SupervisorOptions[P Syncable, M any] struct {
	Name           gen.Atom
	RetryMin       time.Duration
	RetryMax       time.Duration
	ControlTimeout time.Duration
	CatalogOptions CatalogOptions[P]
	SnapshotReader snapshot.ReaderActorOptions
	Projection     snapshot.ProjectionSpec[M]
	Directory      string
	Stopped        chan<- error
}

// CatalogOptions configures one plugin catalog and the routers it spawns.
type CatalogOptions[T Syncable] struct {
	RetryMin      time.Duration // router-level restart backoff
	RetryMax      time.Duration
	RouterOptions RouterOptions[T] // handed straight to each spawned router
}

// RouterOptions configures one deployment router and the managers it spawns.
type RouterOptions[T Syncable] struct {
	Adapter        *Adapter[T]
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

package plugin

import "time"

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

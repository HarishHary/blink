package plugin

import "time"

// DeploymentPoolOptions configures one deployment worker pool.
type DeploymentPoolOptions struct {
	InitialSize int64
	MaxSize     int64
	Worker      DeploymentWorkerOptions
}

// DeploymentWorkerOptions configures one plugin execution worker.
type DeploymentWorkerOptions struct {
	InvocationTimeout time.Duration
	HealthInterval    time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
}

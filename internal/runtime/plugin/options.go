package plugin

import "time"

// DeploymentWorkerOptions configures one plugin execution worker.
type DeploymentWorkerOptions struct {
	InvocationTimeout time.Duration
	HealthInterval    time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
}

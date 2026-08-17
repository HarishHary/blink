package plugin

import "time"

const (
	DefaultWorkerInvocationTimeout = 30 * time.Second
	DefaultWorkerHealthInterval    = 15 * time.Second
	DefaultWorkerRetryMin          = time.Second
	DefaultWorkerRetryMax          = time.Minute
)

// deploymentWorkerOptionsWithDefaults fills worker timing defaults.
func deploymentWorkerOptionsWithDefaults(opts DeploymentWorkerOptions) DeploymentWorkerOptions {
	if opts.InvocationTimeout <= 0 {
		opts.InvocationTimeout = DefaultWorkerInvocationTimeout
	}
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = DefaultWorkerHealthInterval
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultWorkerRetryMin
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = DefaultWorkerRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

package plugin

import "time"

// DefaultDeploymentManagerQueueSize and related constants define default deployment options.
const (
	DefaultDeploymentManagerQueueSize       = 128
	DefaultDeploymentManagerDispatchTimeout = 30 * time.Second
	DefaultDeploymentManagerScaleCooldown   = time.Second
	DefaultDeploymentManagerIdleTimeout     = 30 * time.Second
	DefaultDeploymentManagerDrainTimeout    = 30 * time.Second
	DefaultDeploymentPoolSize               = 1
	DefaultDeploymentPoolRetryMin           = 5 * time.Second
	DefaultDeploymentPoolRetryMax           = 5 * time.Minute
	DefaultWorkerInvocationTimeout          = 30 * time.Second
	DefaultWorkerHealthInterval             = 15 * time.Second
	DefaultWorkerRetryMin                   = time.Second
	DefaultWorkerRetryMax                   = time.Minute
	DefaultCatalogRetryMin                  = DefaultWorkerRetryMin
	DefaultCatalogRetryMax                  = DefaultWorkerRetryMax
	DefaultRouterRetryMin                   = DefaultWorkerRetryMin
	DefaultRouterRetryMax                   = DefaultWorkerRetryMax
)

// catalogOptionsWithDefaults fills catalog and router defaults.
func catalogOptionsWithDefaults[T Syncable](opts CatalogOptions[T]) CatalogOptions[T] {
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultCatalogRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultCatalogRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	opts.RouterOptions = routerOptionsWithDefaults(opts.RouterOptions)
	return opts
}

// routerOptionsWithDefaults fills router and manager defaults.
func routerOptionsWithDefaults[T Syncable](opts RouterOptions[T]) RouterOptions[T] {
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultRouterRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultRouterRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	opts.ManagerOptions = deploymentManagerOptionsWithDefaults(opts.ManagerOptions)
	return opts
}

// deploymentManagerOptionsWithDefaults fills manager and pool defaults.
func deploymentManagerOptionsWithDefaults(opts DeploymentManagerOptions) DeploymentManagerOptions {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultDeploymentManagerQueueSize
	}
	if opts.DispatchTimeout <= 0 {
		opts.DispatchTimeout = DefaultDeploymentManagerDispatchTimeout
	}
	if opts.ScaleCooldown <= 0 {
		opts.ScaleCooldown = DefaultDeploymentManagerScaleCooldown
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultDeploymentManagerIdleTimeout
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDeploymentManagerDrainTimeout
	}
	opts.PoolOptions = deploymentPoolOptionsWithDefaults(opts.PoolOptions)
	return opts
}

// deploymentPoolOptionsWithDefaults fills pool and worker defaults.
func deploymentPoolOptionsWithDefaults(opts DeploymentPoolOptions) DeploymentPoolOptions {
	if opts.InitialSize < 1 {
		opts.InitialSize = DefaultDeploymentPoolSize
	}
	if opts.MaxSize < opts.InitialSize {
		opts.MaxSize = opts.InitialSize
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultDeploymentPoolRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultDeploymentPoolRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	opts.WorkerOptions = deploymentWorkerOptionsWithDefaults(opts.WorkerOptions)
	return opts
}

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

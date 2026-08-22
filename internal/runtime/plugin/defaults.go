package plugin

import (
	goruntime "runtime"
	"time"
)

// MaxDeploymentProcs is the most plugin processes one deployment may declare.
const MaxDeploymentProcs = 100

// DefaultDeploymentManagerQueueSize and related constants define default deployment options.
const (
	DefaultRetryMin                         = 5 * time.Second
	DefaultRetryMax                         = 5 * time.Minute
	DefaultRuntimeMaxConcurrentCalls        = 8  // DefaultRuntimeMaxConcurrentCalls is the number of concurrent caller calls the budgets are sized for, matching the default service concurrency.
	DefaultRuntimeShadowAdmissionShare      = 16 // DefaultRuntimeShadowAdmissionShare divides the production budget into the independent best-effort shadow budget.
	DefaultRuntimeWorkerGrowthPerProc       = 2  // DefaultRuntimeWorkerGrowthPerProc is how many workers past their reservations every usable CPU lets the process grow.
	DefaultRuntimeCloseGracePeriod          = 240 * time.Second
	DefaultDeploymentManagerQueueSize       = 128
	DefaultDeploymentManagerDispatchTimeout = 30 * time.Second
	DefaultDeploymentManagerScaleCooldown   = time.Second
	DefaultDeploymentManagerIdleTimeout     = 30 * time.Second
	DefaultDeploymentManagerDrainTimeout    = 30 * time.Second
	DefaultDeploymentManagerCircuitCooldown = 5 * time.Minute
	DefaultDeploymentPoolSize               = 1
	DefaultDeploymentPoolRetryMin           = DefaultRetryMin
	DefaultDeploymentPoolRetryMax           = DefaultRetryMax
	DefaultWorkerInvocationTimeout          = 120 * time.Second
	DefaultWorkerHealthInterval             = 10 * time.Second
	DefaultWorkerRetryMin                   = DefaultRetryMin
	DefaultWorkerRetryMax                   = DefaultRetryMax
	DefaultSupervisorRetryMin               = DefaultRetryMin
	DefaultSupervisorRetryMax               = DefaultRetryMax
	DefaultSupervisorControlTimeout         = 120 * time.Second
	DefaultCatalogRetryMin                  = DefaultRetryMin
	DefaultCatalogRetryMax                  = DefaultRetryMax
	DefaultRouterRetryMin                   = DefaultRetryMin
	DefaultRouterRetryMax                   = DefaultRetryMax
)

// runtimeOptionsWithDefaults fills public runtime option defaults.
func runtimeOptionsWithDefaults(opts Options) Options {
	if opts.MaxConcurrentCalls <= 0 {
		opts.MaxConcurrentCalls = DefaultRuntimeMaxConcurrentCalls
	}
	// One call fans a batch out to (rollout groups x shards per group) concurrent invocations.
	// Shards fill the deployment's worker count, and the two-way split a partial canary adds
	// costs one call more when those two groups do not divide that count evenly, so one call is
	// at most max_procs + 1 invocations - or the batch's own size, since a shard needs an event
	// to carry. Those are the only two bounds: the batch's tenant spread does not widen a call,
	// so nothing else here can bound one. The per-plugin share rejects instead of
	// waiting, and a rejected call is retried and then dead-lettered, so it has to cover every
	// call the caller runs at once: a share holding one fan-out would fail a legitimate second
	// batch over the same plugin with ErrQueueFull. Nothing here knows how many plugins a batch
	// touches, so the shared budget cannot be derived from one plugin's width; it only blocks, so
	// it is a process-wide ceiling held that many per-plugin shares above a single plugin's.
	pluginFanOut := MaxDeploymentProcs + 1
	if opts.MaxBatchSize > 0 {
		pluginFanOut = min(opts.MaxBatchSize, pluginFanOut)
	}
	if opts.MaxOutstandingInvocationsPerPlugin <= 0 {
		opts.MaxOutstandingInvocationsPerPlugin = pluginFanOut * opts.MaxConcurrentCalls
	}
	if opts.MaxOutstandingInvocations <= 0 {
		opts.MaxOutstandingInvocations = opts.MaxOutstandingInvocationsPerPlugin * opts.MaxConcurrentCalls
	}
	if opts.MaxOutstandingInvocationsPerPlugin > opts.MaxOutstandingInvocations {
		opts.MaxOutstandingInvocationsPerPlugin = opts.MaxOutstandingInvocations
	}
	if opts.ShadowMaxOutstandingInvocations <= 0 {
		opts.ShadowMaxOutstandingInvocations = max(1, opts.MaxOutstandingInvocations/DefaultRuntimeShadowAdmissionShare)
	}
	// That whole fan-out lands on the plugin's single deployment manager, where everything
	// past the running workers waits in its pending queue. A queue below the plugin's
	// admission budget would just move the same rejection one layer down. Only the budget is
	// applied here, since it is the one input a manager cannot see; a budget under the
	// manager's own default leaves the queue to it.
	if opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.QueueSize <= 0 &&
		opts.MaxOutstandingInvocationsPerPlugin > DefaultDeploymentManagerQueueSize {
		opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.QueueSize = opts.MaxOutstandingInvocationsPerPlugin
	}

	// Every worker is a subprocess, and past a deployment's reserved min_procs it only pays off
	// while a core is free to run it: many plugins scaling up at once turn extra workers into
	// contention instead of throughput. One budget therefore bounds that growth across all of
	// them, sized from GOMAXPROCS because it, unlike NumCPU, respects the container's CPU limit.
	if opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.WorkerBudget == nil {
		opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.WorkerBudget = NewWorkerBudget(goruntime.GOMAXPROCS(0) * DefaultRuntimeWorkerGrowthPerProc)
	}
	opts.SupervisorOptions = supervisorOptionsWithDefaults(opts.SupervisorOptions)
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.DrainTimeout + DefaultRuntimeCloseGracePeriod
	}
	return opts
}

// supervisorOptionsWithDefaults fills supervisor and child option defaults.
func supervisorOptionsWithDefaults(opts SupervisorOptions) SupervisorOptions {
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultSupervisorRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultSupervisorRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	if opts.ControlTimeout <= 0 {
		opts.ControlTimeout = DefaultSupervisorControlTimeout
	}
	opts.CatalogOptions = catalogOptionsWithDefaults(opts.CatalogOptions)
	return opts
}

// catalogOptionsWithDefaults fills catalog and router defaults.
func catalogOptionsWithDefaults(opts CatalogOptions) CatalogOptions {
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
func routerOptionsWithDefaults(opts RouterOptions) RouterOptions {
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
	if opts.CircuitCooldown <= 0 {
		opts.CircuitCooldown = DefaultDeploymentManagerCircuitCooldown
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
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultWorkerRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

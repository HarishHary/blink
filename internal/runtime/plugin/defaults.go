package plugin

import (
	goruntime "runtime"
	"time"
)

const (
	// DefaultDeploymentCallsPerProcess is what a process serves when its deployment declares nothing:
	// one call at a time, all an arbitrary plugin can be assumed to survive.
	DefaultDeploymentCallsPerProcess = 1
	// MaxDeploymentCallsPerProcess is the most one process may declare. Each is a goroutine and a gRPC
	// stream inside a subprocess Blink cannot size, so only a benchmark justifies raising it.
	MaxDeploymentCallsPerProcess = 64
	// MaxDeploymentProcs is the most plugin processes one deployment may declare.
	MaxDeploymentProcs = 100
)

// Default option values for the runtime and every child it configures.
const (
	DefaultRetryMin                         = 5 * time.Second
	DefaultRetryMax                         = 5 * time.Minute
	DefaultRuntimeMaxConcurrentCalls        = 8  // concurrent caller calls the budgets are sized for
	DefaultRuntimeShadowAdmissionShare      = 16 // divides the production budget into the shadow one
	DefaultRuntimeProcessGrowthPerProc      = 2  // plugin process growth per usable CPU
	DefaultRuntimeCloseGracePeriod          = 240 * time.Second
	DefaultDeploymentManagerQueueSize       = 128
	DefaultDeploymentManagerDispatchTimeout = 30 * time.Second
	DefaultDeploymentManagerScaleCooldown   = time.Second
	DefaultDeploymentManagerIdleTimeout     = 30 * time.Second
	DefaultDeploymentManagerDrainTimeout    = 30 * time.Second
	DefaultDeploymentManagerCircuitCooldown = 5 * time.Minute
	DefaultDeploymentManagerRetryMin        = DefaultRetryMin
	DefaultDeploymentManagerRetryMax        = DefaultRetryMax
	DefaultPluginProcessInvocationTimeout   = 120 * time.Second
	DefaultPluginProcessHealthInterval      = 10 * time.Second
	DefaultPluginProcessRetryMin            = DefaultRetryMin
	DefaultPluginProcessRetryMax            = DefaultRetryMax
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
	// One call is at most as wide as the deployment capacity it fills, the rollout bucket count, or
	// the batch itself, and this bounds all three. Capacity declared past it arrives as further calls.
	opts.callFanOut = MaxDeploymentProcs + 1
	if opts.MaxBatchSize > 0 {
		opts.callFanOut = min(opts.MaxBatchSize, opts.callFanOut)
	}
	// The per-plugin share rejects rather than waits, so it holds one whole fan-out for every call the
	// caller runs at once. The shared budget only blocks, so it sits that many shares above one share.
	opts.maxOutstandingInvocationsPerPlugin = opts.callFanOut * opts.MaxConcurrentCalls
	opts.maxOutstandingInvocations = opts.maxOutstandingInvocationsPerPlugin * opts.MaxConcurrentCalls
	opts.shadowMaxOutstandingInvocations = max(1, opts.maxOutstandingInvocations/DefaultRuntimeShadowAdmissionShare)
	// One plugin's whole fan-out lands on one manager, so a queue under its budget would move the same
	// rejection a layer down. Under the manager's own default the queue is left to it.
	if opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.QueueSize <= 0 &&
		opts.maxOutstandingInvocationsPerPlugin > DefaultDeploymentManagerQueueSize {
		opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.QueueSize = opts.maxOutstandingInvocationsPerPlugin
	}

	// A subprocess past a deployment's min_procs only pays off while a core is free to run it, so one
	// budget bounds that growth, from GOMAXPROCS because it respects the container's CPU limit.
	if opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.ProcessBudget == nil {
		opts.SupervisorOptions.CatalogOptions.RouterOptions.ManagerOptions.ProcessBudget = NewProcessBudget(goruntime.GOMAXPROCS(0) * DefaultRuntimeProcessGrowthPerProc)
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

// deploymentManagerOptionsWithDefaults fills manager and plugin process defaults.
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
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultDeploymentManagerRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultDeploymentManagerRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	opts.ProcessOptions = pluginProcessOptionsWithDefaults(opts.ProcessOptions)
	return opts
}

// pluginProcessOptionsWithDefaults fills plugin process timing defaults.
func pluginProcessOptionsWithDefaults(opts PluginProcessOptions) PluginProcessOptions {
	if opts.InvocationTimeout <= 0 {
		opts.InvocationTimeout = DefaultPluginProcessInvocationTimeout
	}
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = DefaultPluginProcessHealthInterval
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultPluginProcessRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultPluginProcessRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

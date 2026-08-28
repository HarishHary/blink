package plugin

import (
	"time"

	"ergo.services/ergo/gen"
)

const (
	// DefaultDeploymentCallsPerProcess is what a process serves undeclared; above 1 the plugin has to be
	// concurrency-safe.
	DefaultDeploymentCallsPerProcess = 32
	// MaxDeploymentCallsPerProcess is the most one process may declare, each a goroutine and a stream in
	// a subprocess Blink cannot size.
	MaxDeploymentCallsPerProcess = 64
	// DefaultMaxDeploymentProcs is what a deployment scales to when it declares nothing.
	DefaultMaxDeploymentProcs = 1
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
	DefaultPluginProcessHealthInterval      = 20 * time.Second
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

// Every name below derives from the namespace, mirroring the controller's controller-<namespace>-*
// names, so a caller never has to be told one.

// ApplicationName is the runtime application's registered name.
func ApplicationName(namespace string) gen.Atom { return subtreeName(namespace, "application") }

// SupervisorName is the runtime supervisor's registered name.
func SupervisorName(namespace string) gen.Atom { return subtreeName(namespace, "supervisor") }

// ReconcilerActorName is the reconciler child's registered name.
func ReconcilerActorName(namespace string) gen.Atom { return subtreeName(namespace, "reconciler") }

// CatalogActorName is the catalog child's registered name.
func CatalogActorName(namespace string) gen.Atom { return subtreeName(namespace, "catalog") }

// subtreeName builds one plugin runtime name from its namespace.
func subtreeName(namespace, suffix string) gen.Atom {
	return gen.Atom("plugin-" + namespace + "-" + suffix)
}

// runtimeOptionsWithDefaults fills public runtime option defaults.
func runtimeOptionsWithDefaults(opts ApplicationOptions) ApplicationOptions {
	if opts.MaxConcurrentCalls <= 0 {
		opts.MaxConcurrentCalls = DefaultRuntimeMaxConcurrentCalls
	}
	// One call is at most this wide, and never wider than the batch; capacity past it arrives as further calls.
	opts.callFanOut = MaxDeploymentProcs + 1
	if opts.MaxBatchSize > 0 {
		opts.callFanOut = min(opts.MaxBatchSize, opts.callFanOut)
	}
	// The per-plugin share rejects rather than waits, so it holds a whole fan-out per concurrent call.
	opts.maxOutstandingInvocationsPerPlugin = opts.callFanOut * opts.MaxConcurrentCalls
	// The shared budget only blocks, so it sits that many shares above one plugin's share.
	opts.maxOutstandingInvocations = opts.maxOutstandingInvocationsPerPlugin * opts.MaxConcurrentCalls
	opts.shadowMaxOutstandingInvocations = max(1, opts.maxOutstandingInvocations/DefaultRuntimeShadowAdmissionShare)
	// One plugin's whole fan-out lands on one manager, so a queue under its share would move the same
	// rejection a layer down.
	if opts.SupervisorOptions.CatalogOptions.RouterOptions.DeploymentManagerOptions.QueueSize <= 0 &&
		opts.maxOutstandingInvocationsPerPlugin > DefaultDeploymentManagerQueueSize {
		opts.SupervisorOptions.CatalogOptions.RouterOptions.DeploymentManagerOptions.QueueSize = opts.maxOutstandingInvocationsPerPlugin
	}

	// Growth past a deployment's min_procs: see processBudgetFromResources for why this is sized from
	// CPU and memory together
	if opts.SupervisorOptions.CatalogOptions.RouterOptions.DeploymentManagerOptions.ProcessBudget == nil {
		opts.SupervisorOptions.CatalogOptions.RouterOptions.DeploymentManagerOptions.ProcessBudget = NewProcessBudget(processBudgetFromResources())
	}
	opts.SupervisorOptions = supervisorOptionsWithDefaults(opts.SupervisorOptions)
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = opts.SupervisorOptions.CatalogOptions.RouterOptions.DeploymentManagerOptions.DrainTimeout + DefaultRuntimeCloseGracePeriod
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
	opts.DeploymentManagerOptions = deploymentManagerOptionsWithDefaults(opts.DeploymentManagerOptions)
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
	opts.PluginProcessOptions = pluginProcessOptionsWithDefaults(opts.PluginProcessOptions)
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

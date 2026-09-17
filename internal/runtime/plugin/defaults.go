package plugin

import (
	"fmt"
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
	DefaultDeploymentManagerRestartMin      = DefaultRetryMin
	DefaultDeploymentManagerRestartMax      = DefaultRetryMax
	DefaultPluginProcessInvocationTimeout   = 120 * time.Second
	DefaultPluginProcessHealthInterval      = 20 * time.Second
	DefaultPluginProcessRestartMin          = DefaultRetryMin
	DefaultPluginProcessRestartMax          = DefaultRetryMax
	DefaultSupervisorRetryMin               = DefaultRetryMin
	DefaultSupervisorRetryMax               = DefaultRetryMax
	DefaultSupervisorRestartMin             = DefaultRetryMin
	DefaultSupervisorRestartMax             = DefaultRetryMax
	DefaultSupervisorControlTimeout         = 120 * time.Second
	DefaultCatalogRestartMin                = DefaultRetryMin
	DefaultCatalogRestartMax                = DefaultRetryMax
	DefaultRouterRetryMin                   = DefaultRetryMin
	DefaultRouterRetryMax                   = DefaultRetryMax
	DefaultGatewaySubmitTimeout             = 120 * time.Second
)

// Every name below derives from the namespace, mirroring the controller's controller-<namespace>-*
// names, so a caller never has to be told one.

// ApplicationName is the runtime application's registered name.
func ApplicationName(namespace string) gen.Atom { return subtreeName(namespace, "application") }

// PluginRuntimeName is the branch supervisor's registered name, over the gateway and the runtime it feeds.
func PluginRuntimeName(namespace string) gen.Atom { return subtreeName(namespace, "runtime") }

// GatewayName is the invocation gateway's registered name.
func GatewayName(namespace string) gen.Atom { return subtreeName(namespace, "gateway") }

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

// A registered name addresses a process across supervision lines, where the caller holds no PID: the
// client to the gateway, the gateway to its sibling runtime, the application to both. Within a line a
// parent addresses its children by the PID it spawned and monitors, which a name cannot replace, since a
// name resolves to whichever incarnation holds it now. The two below are that boundary's two halves.

// requireSubtreeName fails a process its spawner did not register under the name the rest of the subtree
// resolves it by, since an unnamed or misnamed process starts fine and then answers nobody.
func requireSubtreeName(process gen.Process, name gen.Atom) error {
	if process.Name() == name {
		return nil
	}
	return fmt.Errorf("process %s registered as %q, want %q", process.PID(), process.Name(), name)
}

// subtreePID resolves one registered subtree name, since a name addresses a process only while it is up,
// and an unreachable part of the runtime is one nothing can be submitted to.
func subtreePID(node gen.Node, name gen.Atom) (gen.PID, error) {
	pid, err := node.ProcessPID(name)
	if err != nil {
		return gen.PID{}, fmt.Errorf("%w: resolve %q: %w", ErrPluginUnavailable, name, err)
	}
	return pid, nil
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
	// The gateway holds the admission budgets, so they are sized from this application's own fan-out and
	// concurrency rather than from the widest a bare gateway would assume.
	opts.GatewayOptions = gatewayBudgetsWithDefaults(opts.GatewayOptions, opts.callFanOut, opts.MaxConcurrentCalls)
	opts.GatewayOptions = gatewayOptionsWithDefaults(opts.GatewayOptions)
	// One plugin's whole fan-out lands on one manager, so a queue under the gateway's effective share,
	// derived here or set by the caller, would move the same rejection a layer down.
	share := opts.GatewayOptions.MaxOutstandingInvocationsPerPlugin
	if opts.SupervisorOptions.CatalogOptions.RouterOptions.DeploymentManagerOptions.QueueSize <= 0 &&
		share > DefaultDeploymentManagerQueueSize {
		opts.SupervisorOptions.CatalogOptions.RouterOptions.DeploymentManagerOptions.QueueSize = share
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

// gatewayBudgetsWithDefaults sizes the admission budgets a caller left unset from how wide one call may
// fan out and how many calls that caller runs at once, since a budget set apart from the fan-out it has
// to hold is a budget that rejects a legitimate call.
func gatewayBudgetsWithDefaults(opts GatewayOptions, fanOut, concurrent int) GatewayOptions {
	// The per-plugin share rejects rather than waits, so it holds a whole fan-out per concurrent call.
	if opts.MaxOutstandingInvocationsPerPlugin <= 0 {
		opts.MaxOutstandingInvocationsPerPlugin = fanOut * concurrent
	}
	// The shared budget only blocks, so it sits that many shares above one plugin's share.
	if opts.MaxOutstandingInvocations <= 0 {
		opts.MaxOutstandingInvocations = opts.MaxOutstandingInvocationsPerPlugin * concurrent
	}
	if opts.ShadowMaxOutstandingInvocations <= 0 {
		opts.ShadowMaxOutstandingInvocations = max(1, opts.MaxOutstandingInvocations/DefaultRuntimeShadowAdmissionShare)
	}
	return opts
}

// gatewayOptionsWithDefaults fills gateway defaults for a gateway configured on its own, which has no
// application knobs to size from and so assumes the widest fan-out one call is allowed.
func gatewayOptionsWithDefaults(opts GatewayOptions) GatewayOptions {
	opts = gatewayBudgetsWithDefaults(opts, MaxDeploymentProcs+1, DefaultRuntimeMaxConcurrentCalls)
	// Waiting is as wide as the production budget: a shorter queue would reject a caller that only has to
	// wait, and a longer one would hold callers past the point their own deadlines survive.
	if opts.MaxWaiting <= 0 {
		opts.MaxWaiting = opts.MaxOutstandingInvocations
	}
	if opts.SubmitTimeout <= 0 {
		opts.SubmitTimeout = DefaultGatewaySubmitTimeout
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
	if opts.RestartMin <= 0 {
		opts.RestartMin = opts.RetryMin
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = opts.RetryMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	if opts.ControlTimeout <= 0 {
		opts.ControlTimeout = DefaultSupervisorControlTimeout
	}
	opts.CatalogOptions = catalogOptionsWithDefaults(opts.CatalogOptions)
	return opts
}

// catalogOptionsWithDefaults fills catalog and router defaults.
func catalogOptionsWithDefaults(opts CatalogOptions) CatalogOptions {
	if opts.RestartMin <= 0 {
		opts.RestartMin = DefaultCatalogRestartMin
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = DefaultCatalogRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
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
	if opts.RestartMin <= 0 {
		opts.RestartMin = DefaultDeploymentManagerRestartMin
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = DefaultDeploymentManagerRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
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
	if opts.RestartMin <= 0 {
		opts.RestartMin = DefaultPluginProcessRestartMin
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = DefaultPluginProcessRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	return opts
}

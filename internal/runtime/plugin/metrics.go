package plugin

import (
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// Radar metric names by publishing layer. The namespace label carries the controller namespace this
// runtime follows, the same value the controller's and the snapshot subtree's series carry, so all
// three sides join on it.

// Supervisor series: every gauge, since the supervisor is the one process holding what the
// reconciler, the catalog, and every route below it report. A gauge labelled by namespace alone
// published from a router or a manager too would be overwritten by whichever one sampled last.
const (
	metricSupervisorLifecycle    = "blink_plugin_supervisor_lifecycle"
	metricAvailability           = "blink_plugin_availability"
	metricTransition             = "blink_plugin_transition"
	metricDesiredRevision        = "blink_plugin_desired_revision"
	metricReadyGeneration        = "blink_plugin_projection_ready_generation"
	metricCommittedGeneration    = "blink_plugin_projection_committed_generation"
	metricInFlightCalls          = "blink_plugin_in_flight_calls"
	metricReconcilerAvailability = "blink_plugin_reconciler_availability"
	metricReconcilerGeneration   = "blink_plugin_reconciler_generation"
	metricReconcilerRevision     = "blink_plugin_reconciler_revision"
	metricCatalogAvailability    = "blink_plugin_catalog_availability"
	metricRoutersDesired         = "blink_plugin_routers_desired"
	metricRoutersRoutable        = "blink_plugin_routers_routable"
	metricRoutersSettled         = "blink_plugin_routers_settled"
	metricRoutersUnavailable     = "blink_plugin_routers_unavailable"
	metricProcessesReady         = "blink_plugin_processes_ready"
	metricProcessesDesired       = "blink_plugin_processes_desired"
	metricQueueDepth             = "blink_plugin_queue_depth"
	metricActiveCalls            = "blink_plugin_active_calls"
	metricChildStarts            = "blink_plugin_child_starts_total"
	metricChildTerminations      = "blink_plugin_child_terminations_total"
	metricInvocations            = "blink_plugin_invocations_total"
	metricInvocationsRejected    = "blink_plugin_invocations_rejected_total"
	metricProjectionCommits      = "blink_plugin_projection_commits_total"
	metricPromotions             = "blink_plugin_desired_state_promotions_total"
)

// Reconciler series: resolving the snapshot and the artifact directory into desired state.
const (
	metricResolutions       = "blink_plugin_resolutions_total"
	metricResolutionRetries = "blink_plugin_resolution_retries_total"
	metricWorkerRestarts    = "blink_plugin_artifact_worker_restarts_total"
)

// Catalog series: the router this runtime keeps per plugin.
const (
	metricRouterStarts       = "blink_plugin_router_starts_total"
	metricRouterRestarts     = "blink_plugin_router_restarts_total"
	metricRouterTerminations = "blink_plugin_router_terminations_total"
)

// Router series: the rollout decision one invocation takes.
const (
	metricRouted             = "blink_plugin_routed_total"
	metricUnroutable         = "blink_plugin_unroutable_total"
	metricAcceptanceTimeouts = "blink_plugin_acceptance_timeouts_total"
)

// Deployment manager series: queueing, dispatch, and the plugin processes serving one deployment.
const (
	metricQueueRejects        = "blink_plugin_queue_rejects_total"
	metricDispatchTimeouts    = "blink_plugin_dispatch_timeouts_total"
	metricProcessStarts       = "blink_plugin_process_starts_total"
	metricProcessRestarts     = "blink_plugin_process_restarts_total"
	metricProcessTerminations = "blink_plugin_process_terminations_total"
	metricCircuitOpens        = "blink_plugin_circuit_opens_total"
	metricScaleEvents         = "blink_plugin_scale_events_total"
	metricInvocationTime      = "blink_plugin_invocation_seconds"
)

var (
	namespaceLabels = []string{"namespace"}
	resultLabels    = []string{"namespace", "result"}
	reasonLabels    = []string{"namespace", "reason"}
	// invocationBuckets span a trivial call to one that spends the whole invocation timeout in a
	// subprocess, queueing included.
	invocationBuckets = []float64{0.005, 0.025, 0.1, 0.5, 1, 5, 15, 30, 60, 120}
	runtimeMetrics    = []telemetry.MetricSpec{
		// supervisor
		{Kind: telemetry.Gauge, Name: metricSupervisorLifecycle, Help: "Supervisor lifecycle: 0 starting, 1 running, 2 draining", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricAvailability, Help: "Runtime availability: 0 unavailable, 1 degraded, 2 ready", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricTransition, Help: "Desired-state transition: 0 idle, 1 preparing, 2 awaiting freshness, 3 awaiting projection", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricDesiredRevision, Help: "Newest applied or pending desired-state revision", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricReadyGeneration, Help: "Projection generation this runtime admits calls against", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricCommittedGeneration, Help: "Projection generation this runtime has asked the snapshot subtree to commit", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricInFlightCalls, Help: "Invocations the runtime supervisor tracks, which is what a transition waits on", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricReconcilerAvailability, Help: "Reconciler availability: 0 unavailable, 1 degraded, 2 ready", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricReconcilerGeneration, Help: "Snapshot generation the reconciler resolved against", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricReconcilerRevision, Help: "Desired-state revision the reconciler has proposed", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricCatalogAvailability, Help: "Catalog availability: 0 unavailable, 1 degraded, 2 ready", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricRoutersDesired, Help: "Plugins the current revision asks this runtime to route", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricRoutersRoutable, Help: "Routers accepting invocations", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricRoutersSettled, Help: "Routers done moving toward the current revision, whether they route or failed for good", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricRoutersUnavailable, Help: "Routers serving nothing", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricProcessesReady, Help: "Plugin processes serving invocations across every route", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricProcessesDesired, Help: "Plugin processes every route wants running", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricQueueDepth, Help: "Invocations queued for process capacity across every route", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricActiveCalls, Help: "Invocations executing in a plugin process across every route", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricChildStarts, Help: "Supervised children started, by child", Labels: []string{"namespace", "child"}},
		{Kind: telemetry.Counter, Name: metricChildTerminations, Help: "Supervised children exited, by child and reason", Labels: []string{"namespace", "child", "reason"}},
		{Kind: telemetry.Counter, Name: metricInvocations, Help: "Invocations completed by result", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricInvocationsRejected, Help: "Invocations refused at admission, by reason", Labels: reasonLabels},
		{Kind: telemetry.Counter, Name: metricProjectionCommits, Help: "Projection commit requests by result", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricPromotions, Help: "Desired-state revisions promoted into the catalog", Labels: namespaceLabels},

		// reconciler
		{Kind: telemetry.Counter, Name: metricResolutions, Help: "Artifact resolutions by outcome: proposed, unchanged, deferred, or stale", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricResolutionRetries, Help: "Delayed re-resolutions scheduled after a deferred or failed one", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricWorkerRestarts, Help: "Artifact meta-process restarts, by worker", Labels: []string{"namespace", "worker"}},

		// catalog
		{Kind: telemetry.Counter, Name: metricRouterStarts, Help: "Router actors spawned", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricRouterRestarts, Help: "Router restarts scheduled after a loss", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricRouterTerminations, Help: "Router actors exited, by reason", Labels: reasonLabels},

		// router
		{Kind: telemetry.Counter, Name: metricRouted, Help: "Invocations routed, by rollout target", Labels: []string{"namespace", "target"}},
		{Kind: telemetry.Counter, Name: metricUnroutable, Help: "Invocations with no route to take", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricAcceptanceTimeouts, Help: "Routed invocations no manager acknowledged in time", Labels: namespaceLabels},

		// deployment manager
		{Kind: telemetry.Counter, Name: metricQueueRejects, Help: "Invocations refused because a deployment's queue was full", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricDispatchTimeouts, Help: "Dispatched invocations no plugin process started in time", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricProcessStarts, Help: "Plugin processes spawned", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricProcessRestarts, Help: "Plugin process restarts scheduled against a slot's retry budget", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricProcessTerminations, Help: "Plugin processes exited, by reason", Labels: reasonLabels},
		{Kind: telemetry.Counter, Name: metricCircuitOpens, Help: "Deployments whose circuit opened on a spent restart budget", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricScaleEvents, Help: "Autoscaling decisions, by direction", Labels: []string{"namespace", "direction"}},
		{
			Kind: telemetry.Histogram, Name: metricInvocationTime, Labels: namespaceLabels,
			Help:    "Seconds from a deployment manager accepting one invocation to completing it, queueing included",
			Buckets: invocationBuckets,
		},
	}
)

// runtimeGauges is every gauge one plugin runtime publishes, one per field.
type runtimeGauges struct {
	lifecycle              SupervisorLifecycle
	availability           runtime.Availability
	transition             SupervisorTransitionPhase
	desiredRevision        uint64
	readyGeneration        int64
	committedGeneration    int64
	inFlightCalls          int
	reconcilerAvailability runtime.Availability
	reconcilerGeneration   int64
	reconcilerRevision     uint64
	catalogAvailability    runtime.Availability
	routersDesired         int
	routersRoutable        int
	routersSettled         int
	routersUnavailable     int
	processesReady         int
	processesDesired       int
	queueDepth             int
	activeCalls            int
}

// publish reports the runtime's gauges; the supervisor republishes them on its radar tick so a
// converged runtime still reports fresh series.
func (g runtimeGauges) publish(labels telemetry.Labels, sender telemetry.Sender) {
	labels.Set(sender, metricSupervisorLifecycle, supervisorLifecycleValue(g.lifecycle))
	labels.Set(sender, metricAvailability, telemetry.AvailabilityValue(g.availability))
	labels.Set(sender, metricTransition, float64(g.transition))
	labels.Set(sender, metricDesiredRevision, float64(g.desiredRevision))
	labels.Set(sender, metricReadyGeneration, float64(g.readyGeneration))
	labels.Set(sender, metricCommittedGeneration, float64(g.committedGeneration))
	labels.Set(sender, metricInFlightCalls, float64(g.inFlightCalls))
	labels.Set(sender, metricReconcilerAvailability, telemetry.AvailabilityValue(g.reconcilerAvailability))
	labels.Set(sender, metricReconcilerGeneration, float64(g.reconcilerGeneration))
	labels.Set(sender, metricReconcilerRevision, float64(g.reconcilerRevision))
	labels.Set(sender, metricCatalogAvailability, telemetry.AvailabilityValue(g.catalogAvailability))
	labels.Set(sender, metricRoutersDesired, float64(g.routersDesired))
	labels.Set(sender, metricRoutersRoutable, float64(g.routersRoutable))
	labels.Set(sender, metricRoutersSettled, float64(g.routersSettled))
	labels.Set(sender, metricRoutersUnavailable, float64(g.routersUnavailable))
	labels.Set(sender, metricProcessesReady, float64(g.processesReady))
	labels.Set(sender, metricProcessesDesired, float64(g.processesDesired))
	labels.Set(sender, metricQueueDepth, float64(g.queueDepth))
	labels.Set(sender, metricActiveCalls, float64(g.activeCalls))
}

// supervisorLifecycleValue orders the lifecycle so a dashboard reads it as progress towards a stop.
func supervisorLifecycleValue(lifecycle SupervisorLifecycle) float64 {
	switch lifecycle {
	case SupervisorRunning:
		return 1
	case SupervisorDraining:
		return 2
	default:
		return 0
	}
}

package snapshot

import (
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// Radar metric names by publishing layer. The namespace label carries the controller namespace this
// subtree follows, the same value the controller's own series carry, so the two sides join on it.

// Supervisor series: every gauge, since the supervisor is the one process holding both children's status.
const (
	metricSupervisorLifecycle    = "blink_snapshot_supervisor_lifecycle"
	metricReaderAvailability     = "blink_snapshot_reader_availability"
	metricReaderGeneration       = "blink_snapshot_reader_generation"
	metricProjectionAvailability = "blink_snapshot_projection_availability"
	metricCommittedGeneration    = "blink_snapshot_projection_committed_generation"
	metricPreparedGeneration     = "blink_snapshot_projection_prepared_generation"
	metricReportedAvailability   = "blink_snapshot_reported_availability"
	metricGenerationLag          = "blink_snapshot_generation_lag"
	metricCommitPending          = "blink_snapshot_commit_pending"
	metricExecutorReports        = "blink_snapshot_executor_reports_total"
	metricChildStarts            = "blink_snapshot_child_starts_total"
	metricChildTerminations      = "blink_snapshot_child_terminations_total"
)

// Reader series: this executor's subscription to its controller.
const (
	metricSubscribeAttempts = "blink_snapshot_subscribe_attempts_total"
	metricUpdates           = "blink_snapshot_updates_total"
	metricUpdatesIgnored    = "blink_snapshot_updates_ignored_total"
	metricControllerDown    = "blink_snapshot_controller_down_total"
)

// Projection series: parsing a received generation, and committing one in external mode.
const (
	metricParses        = "blink_snapshot_parses_total"
	metricParseFailures = "blink_snapshot_parse_failures_total"
	metricParseTime     = "blink_snapshot_parse_seconds"
	metricCommits       = "blink_snapshot_commits_total"
)

var (
	namespaceLabels = []string{"namespace"}
	resultLabels    = []string{"namespace", "result"}
	// parseBuckets span one small spec to a full catalog of them, all in-process work.
	parseBuckets   = []float64{0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1}
	subtreeMetrics = []telemetry.MetricSpec{
		// supervisor
		{Kind: telemetry.Gauge, Name: metricSupervisorLifecycle, Help: "Supervisor lifecycle: 0 starting, 1 running, 2 stopping", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricReaderAvailability, Help: "Reader availability: 0 unavailable, 1 degraded, 2 ready", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricReaderGeneration, Help: "Newest generation the controller has delivered", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricProjectionAvailability, Help: "Projection availability: 0 unavailable, 1 degraded, 2 ready", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricCommittedGeneration, Help: "Generation the projection currently serves", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricPreparedGeneration, Help: "Generation parsed and awaiting an external commit, 0 when none", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricReportedAvailability, Help: "Availability this executor reports to its controller: 0 unavailable, 1 degraded, 2 ready", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricGenerationLag, Help: "Generations delivered but not yet serving, the local half of controller-side drift", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricCommitPending, Help: "Generation whose external commit is in flight, 0 when none", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricExecutorReports, Help: "Convergence reports sent to the controller", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricChildStarts, Help: "Supervised children started, by child", Labels: []string{"namespace", "child"}},
		{Kind: telemetry.Counter, Name: metricChildTerminations, Help: "Supervised children exited, by child and reason", Labels: []string{"namespace", "child", "reason"}},

		// reader
		{Kind: telemetry.Counter, Name: metricSubscribeAttempts, Help: "Subscribe calls to the controller by result", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricUpdates, Help: "Pushed snapshot updates accepted", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricUpdatesIgnored, Help: "Pushed snapshot updates dropped, by reason", Labels: []string{"namespace", "reason"}},
		{Kind: telemetry.Counter, Name: metricControllerDown, Help: "Losses of the controller, by what went down", Labels: []string{"namespace", "scope"}},

		// projection
		{Kind: telemetry.Counter, Name: metricParses, Help: "Generations parsed by result: ok, partial, or failed", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricParseFailures, Help: "Individual specs a generation could not parse", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricCommits, Help: "External commit requests by result", Labels: resultLabels},
		{
			Kind: telemetry.Histogram, Name: metricParseTime, Labels: namespaceLabels,
			Help:    "Seconds spent parsing one received generation",
			Buckets: parseBuckets,
		},
	}
)

// subtreeGauges is every gauge one snapshot subtree publishes, one per field.
type subtreeGauges struct {
	lifecycle              SupervisorLifecycle
	readerAvailability     runtime.Availability
	readerGeneration       int64
	projectionAvailability runtime.Availability
	committedGeneration    int64
	preparedGeneration     int64
	reportedAvailability   runtime.Availability
	generationLag          int64
	commitPending          int64
}

// publish reports the subtree's gauges; the supervisor republishes them on its report tick so a
// converged subtree still reports fresh series.
func (g subtreeGauges) publish(labels telemetry.Labels, sender telemetry.Sender) {
	labels.Set(sender, metricSupervisorLifecycle, supervisorLifecycleValue(g.lifecycle))
	labels.Set(sender, metricReaderAvailability, telemetry.AvailabilityValue(g.readerAvailability))
	labels.Set(sender, metricReaderGeneration, float64(g.readerGeneration))
	labels.Set(sender, metricProjectionAvailability, telemetry.AvailabilityValue(g.projectionAvailability))
	labels.Set(sender, metricCommittedGeneration, float64(g.committedGeneration))
	labels.Set(sender, metricPreparedGeneration, float64(g.preparedGeneration))
	labels.Set(sender, metricReportedAvailability, telemetry.AvailabilityValue(g.reportedAvailability))
	labels.Set(sender, metricGenerationLag, float64(g.generationLag))
	labels.Set(sender, metricCommitPending, float64(g.commitPending))
}

// supervisorLifecycleValue orders the lifecycle so a dashboard reads it as progress towards a stop.
func supervisorLifecycleValue(lifecycle SupervisorLifecycle) float64 {
	switch lifecycle {
	case SupervisorRunning:
		return 1
	case SupervisorStopping:
		return 2
	default:
		return 0
	}
}

package controller

import (
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// Radar metric names by publishing layer; the namespace is a label, not part of any name.

// Actor series: what the one controller actor per namespace knows about its own catalog.
const (
	metricAvailability      = "blink_controller_availability"
	metricGeneration        = "blink_controller_generation"
	metricRecords           = "blink_controller_records"
	metricSubscribers       = "blink_controller_subscribers"
	metricExecutors         = "blink_controller_executors"
	metricExecutorsDrifting = "blink_controller_executors_drifting"
	metricSnapshotCommits   = "blink_controller_snapshot_commits_total"
	metricSnapshotWrites    = "blink_controller_snapshot_writes_total"
	metricSnapshotWriteTime = "blink_controller_snapshot_write_seconds"
	metricArtifactScans     = "blink_controller_artifact_scans_total"
	metricWorkerRestarts    = "blink_controller_worker_restarts_total"
)

// Meta series: the scanner's and writer's own filesystem and database work.
const (
	metricArtifactScanTime     = "blink_controller_artifact_scan_seconds"
	metricArtifactSpecs        = "blink_controller_artifact_specs"
	metricArtifactBinaries     = "blink_controller_artifact_binaries"
	metricArtifactScanFailures = "blink_controller_artifact_scan_failures_total"
	metricSnapshotLoadTime     = "blink_controller_snapshot_load_seconds"
	metricSnapshotLoads        = "blink_controller_snapshot_loads_total"
	metricWriteQueue           = "blink_controller_snapshot_write_queue"
	metricWriteQueueRejects    = "blink_controller_snapshot_write_rejects_total"
	metricWriteAttempts        = "blink_controller_snapshot_write_attempts_total"
)

// Supervisor series: the layer that stays up while the actor restarts underneath it.
const (
	metricSupervisorLifecycle = "blink_controller_supervisor_lifecycle"
	metricWriterFences        = "blink_controller_writer_fences"
	metricChildStarts         = "blink_controller_child_starts_total"
	metricChildTerminations   = "blink_controller_child_terminations_total"
)

// Application series: resource ownership, which outlives every supervisor incarnation.
const (
	metricApplicationState        = "blink_controller_application_state"
	metricApplicationLoads        = "blink_controller_application_loads_total"
	metricApplicationTerminations = "blink_controller_application_terminations_total"
	metricApplicationCloses       = "blink_controller_application_closes_total"
)

var (
	namespaceLabels = []string{"namespace"}
	resultLabels    = []string{"namespace", "result"}
	// ioBuckets span an uncontended SQLite write to a contended one; scanBuckets a listing to a full reparse.
	ioBuckets         = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	scanBuckets       = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5}
	controllerMetrics = []telemetry.MetricSpec{
		// actor
		{Kind: telemetry.Gauge, Name: metricAvailability, Help: "Controller availability: 0 unavailable, 1 degraded, 2 ready", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricGeneration, Help: "Committed snapshot generation", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricRecords, Help: "Tracked controller records", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricSubscribers, Help: "Subscribed executors", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricExecutors, Help: "Executors reporting convergence", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricExecutorsDrifting, Help: "Executors behind the committed generation past the drift grace", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricSnapshotCommits, Help: "Snapshot generations committed and pushed to subscribers", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricSnapshotWrites, Help: "Snapshot write attempts by result", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricArtifactScans, Help: "Artifact scan results by outcome", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricWorkerRestarts, Help: "Worker meta-process restarts scheduled", Labels: []string{"namespace", "worker"}},
		{
			Kind: telemetry.Histogram, Name: metricSnapshotWriteTime, Labels: namespaceLabels,
			Help:    "Seconds from dispatching a snapshot write to its result",
			Buckets: ioBuckets,
		},

		// meta: artifact scanner
		{Kind: telemetry.Gauge, Name: metricArtifactSpecs, Help: "Artifact specs the scanner currently holds parsed", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricArtifactBinaries, Help: "Artifact binaries the scanner currently holds checksummed", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricArtifactScanFailures, Help: "Artifact files the scanner could not index, by stage", Labels: []string{"namespace", "stage"}},
		{
			Kind: telemetry.Histogram, Name: metricArtifactScanTime, Labels: namespaceLabels,
			Help:    "Seconds one artifact directory scan took",
			Buckets: scanBuckets,
		},

		// meta: snapshot writer
		{Kind: telemetry.Gauge, Name: metricWriteQueue, Help: "Snapshot writes queued in the writer", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricWriteQueueRejects, Help: "Snapshot writes rejected because the writer queue was full", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricWriteAttempts, Help: "Individual writer database attempts by result, including retries", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricSnapshotLoads, Help: "Writer startup loads of persisted state by result", Labels: resultLabels},
		{
			Kind: telemetry.Histogram, Name: metricSnapshotLoadTime, Labels: namespaceLabels,
			Help:    "Seconds the writer spent loading persisted state at startup",
			Buckets: ioBuckets,
		},

		// supervisor
		{Kind: telemetry.Gauge, Name: metricSupervisorLifecycle, Help: "Supervisor lifecycle: 0 starting, 1 running, 2 draining, 3 stopping", Labels: namespaceLabels},
		{Kind: telemetry.Gauge, Name: metricWriterFences, Help: "Writer I/O fences a drain is still waiting on", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricChildStarts, Help: "Controller actors the supervisor has started", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricChildTerminations, Help: "Controller actor exits by reason", Labels: []string{"namespace", "reason"}},

		// application
		{Kind: telemetry.Gauge, Name: metricApplicationState, Help: "Application state: 0 stopped, 1 loaded", Labels: namespaceLabels},
		{Kind: telemetry.Counter, Name: metricApplicationLoads, Help: "Application load attempts by result", Labels: resultLabels},
		{Kind: telemetry.Counter, Name: metricApplicationTerminations, Help: "Application terminations by reason", Labels: []string{"namespace", "reason"}},
		{Kind: telemetry.Counter, Name: metricApplicationCloses, Help: "Application resource closes by result", Labels: resultLabels},
	}
)

// actorGauges is the controller actor's own state plus its executor tallies, one gauge per field.
type actorGauges struct {
	availability runtime.Availability
	generation   int64
	records      int
	subscribers  int
	executors    int
	drifting     int
}

// publish reports the actor's gauges, republished on its drift-check tick to keep the series fresh.
func (g actorGauges) publish(labels telemetry.Labels, sender telemetry.Sender) {
	labels.Set(sender, metricAvailability, telemetry.AvailabilityValue(g.availability))
	labels.Set(sender, metricGeneration, float64(g.generation))
	labels.Set(sender, metricRecords, float64(g.records))
	labels.Set(sender, metricSubscribers, float64(g.subscribers))
	labels.Set(sender, metricExecutors, float64(g.executors))
	labels.Set(sender, metricExecutorsDrifting, float64(g.drifting))
}

// newHealthSignal names this namespace's readiness signal; radar reads a signal-less node as healthy,
// so it is held down on a drain rather than unregistered.
func newHealthSignal(namespace string) telemetry.Signal {
	return telemetry.NewSignal(gen.Atom("controller-" + namespace))
}

// supervisorLifecycleValue orders the lifecycle so a dashboard reads it as progress towards a stop.
func supervisorLifecycleValue(lifecycle SupervisorLifecycle) float64 {
	switch lifecycle {
	case SupervisorRunning:
		return 1
	case SupervisorDraining:
		return 2
	case SupervisorStopping:
		return 3
	default:
		return 0
	}
}

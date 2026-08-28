package controller

import (
	"errors"
	"fmt"
	"time"

	"ergo.services/actor/metrics"
	"ergo.services/application/radar"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

// Radar's own process names, which the radar package keeps private. The controller needs them
// because only its actor and supervisor are the gen.Process radar's helpers accept: a meta process
// has no Call and an application has no process at all, so both talk to these directly.
const (
	radarHealthProcess  = gen.Atom("radar_health")
	radarMetricsProcess = gen.Atom("radar_metrics")
)

// Radar metric names, grouped by the layer that publishes them. The namespace is a label rather
// than part of any name, so one query covers every controller on the node.

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

// Meta series: the artifact scanner's and snapshot writer's own filesystem and database work, which
// the actor only ever sees as a result message.
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

// healthHeartbeatInterval is how often the supervisor proves it is still running; healthSignalTimeout
// is the deadline radar applies, three beats so one missed tick does not flip readiness on its own.
const (
	healthHeartbeatInterval = 30 * time.Second
	healthSignalTimeout     = 3 * healthHeartbeatInterval
)

type metricKind int

const (
	metricGauge metricKind = iota
	metricCounter
	metricHistogram
)

type metricSpec struct {
	kind    metricKind
	name    string
	help    string
	labels  []string
	buckets []float64
}

// metricRegistrar is the Call half of a process, which is all creating a collector needs. gen.Node
// satisfies it too, and registering through the node is what makes these collectors outlive process
// churn: radar deletes a metric when the process that registered it dies.
type metricRegistrar interface {
	Call(to any, request any) (any, error)
}

// metricSender is the Send half of a process. gen.Process, gen.MetaProcess, and gen.Node all
// satisfy it, so any layer of the controller can publish into a collector another one registered.
type metricSender interface {
	Send(to any, message any) error
}

var metricTypes = map[metricKind]metrics.MetricType{
	metricGauge:     metrics.MetricGauge,
	metricCounter:   metrics.MetricCounter,
	metricHistogram: metrics.MetricHistogram,
}

// register creates the metric on radar's registry; radar accepts a repeat registration of the same
// type and labels, so this is idempotent.
func (m metricSpec) register(registrar metricRegistrar) error {
	result, err := registrar.Call(radarMetricsProcess, metrics.RegisterRequest{
		Name:    m.name,
		Help:    m.help,
		Type:    metricTypes[m.kind],
		Labels:  m.labels,
		Buckets: m.buckets,
	})
	if err != nil {
		return err
	}
	response, ok := result.(metrics.RegisterResponse)
	if !ok {
		return fmt.Errorf("radar metrics: unexpected response %T", result)
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

var (
	namespaceLabels = []string{"namespace"}
	resultLabels    = []string{"namespace", "result"}
	// ioBuckets span a local SQLite write and a contended one; scanBuckets a directory listing and
	// a full reparse of every spec.
	ioBuckets         = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	scanBuckets       = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5}
	controllerMetrics = []metricSpec{
		// actor
		{kind: metricGauge, name: metricAvailability, help: "Controller availability: 0 unavailable, 1 degraded, 2 ready", labels: namespaceLabels},
		{kind: metricGauge, name: metricGeneration, help: "Committed snapshot generation", labels: namespaceLabels},
		{kind: metricGauge, name: metricRecords, help: "Tracked controller records", labels: namespaceLabels},
		{kind: metricGauge, name: metricSubscribers, help: "Subscribed executors", labels: namespaceLabels},
		{kind: metricGauge, name: metricExecutors, help: "Executors reporting convergence", labels: namespaceLabels},
		{kind: metricGauge, name: metricExecutorsDrifting, help: "Executors behind the committed generation past the drift grace", labels: namespaceLabels},
		{kind: metricCounter, name: metricSnapshotCommits, help: "Snapshot generations committed and pushed to subscribers", labels: namespaceLabels},
		{kind: metricCounter, name: metricSnapshotWrites, help: "Snapshot write attempts by result", labels: resultLabels},
		{kind: metricCounter, name: metricArtifactScans, help: "Artifact scan results by outcome", labels: resultLabels},
		{kind: metricCounter, name: metricWorkerRestarts, help: "Worker meta-process restarts scheduled", labels: []string{"namespace", "worker"}},
		{
			kind: metricHistogram, name: metricSnapshotWriteTime, labels: namespaceLabels,
			help:    "Seconds from dispatching a snapshot write to its result",
			buckets: ioBuckets,
		},

		// meta: artifact scanner
		{kind: metricGauge, name: metricArtifactSpecs, help: "Artifact specs the scanner currently holds parsed", labels: namespaceLabels},
		{kind: metricGauge, name: metricArtifactBinaries, help: "Artifact binaries the scanner currently holds checksummed", labels: namespaceLabels},
		{kind: metricCounter, name: metricArtifactScanFailures, help: "Artifact files the scanner could not index, by stage", labels: []string{"namespace", "stage"}},
		{
			kind: metricHistogram, name: metricArtifactScanTime, labels: namespaceLabels,
			help:    "Seconds one artifact directory scan took",
			buckets: scanBuckets,
		},

		// meta: snapshot writer
		{kind: metricGauge, name: metricWriteQueue, help: "Snapshot writes queued in the writer", labels: namespaceLabels},
		{kind: metricCounter, name: metricWriteQueueRejects, help: "Snapshot writes rejected because the writer queue was full", labels: namespaceLabels},
		{kind: metricCounter, name: metricWriteAttempts, help: "Individual writer database attempts by result, including retries", labels: resultLabels},
		{kind: metricCounter, name: metricSnapshotLoads, help: "Writer startup loads of persisted state by result", labels: resultLabels},
		{
			kind: metricHistogram, name: metricSnapshotLoadTime, labels: namespaceLabels,
			help:    "Seconds the writer spent loading persisted state at startup",
			buckets: ioBuckets,
		},

		// supervisor
		{kind: metricGauge, name: metricSupervisorLifecycle, help: "Supervisor lifecycle: 0 starting, 1 running, 2 draining, 3 stopping", labels: namespaceLabels},
		{kind: metricGauge, name: metricWriterFences, help: "Writer I/O fences a drain is still waiting on", labels: namespaceLabels},
		{kind: metricCounter, name: metricChildStarts, help: "Controller actors the supervisor has started", labels: namespaceLabels},
		{kind: metricCounter, name: metricChildTerminations, help: "Controller actor exits by reason", labels: []string{"namespace", "reason"}},

		// application
		{kind: metricGauge, name: metricApplicationState, help: "Application state: 0 stopped, 1 loaded", labels: namespaceLabels},
		{kind: metricCounter, name: metricApplicationLoads, help: "Application load attempts by result", labels: resultLabels},
		{kind: metricCounter, name: metricApplicationTerminations, help: "Application terminations by reason", labels: []string{"namespace", "reason"}},
		{kind: metricCounter, name: metricApplicationCloses, help: "Application resource closes by result", labels: resultLabels},
	}
)

// registerMetrics creates every controller collector. Always call it with the node: radar records
// the registering PID as owner and deletes that owner's collectors when it dies, so registering as
// the actor would drop the supervisor's and application's series on every actor restart.
func registerMetrics(registrar metricRegistrar) error {
	if registrar == nil {
		return errors.New("radar metrics: no registrar")
	}
	for _, metric := range controllerMetrics {
		if err := metric.register(registrar); err != nil {
			return err
		}
	}
	return nil
}

// metricScope publishes one namespace's samples. It holds no registration state, so any layer can
// carry a copy - including a meta process, which cannot register anything itself.
type metricScope struct{ labels []string }

func newMetricScope(namespace string) metricScope {
	return metricScope{labels: []string{namespace}}
}

// set publishes a gauge value.
func (s metricScope) set(sender metricSender, name string, value float64) {
	s.emit(sender, metrics.MessageGaugeSet{Name: name, Value: value, Labels: s.labels})
}

// count increments a counter, appending any label values the metric declares after the namespace.
func (s metricScope) count(sender metricSender, name string, labels ...string) {
	s.emit(sender, metrics.MessageCounterAdd{Name: name, Value: 1, Labels: s.values(labels...)})
}

// observe records one histogram sample.
func (s metricScope) observe(sender metricSender, name string, value float64) {
	s.emit(sender, metrics.MessageHistogramObserve{Name: name, Value: value, Labels: s.labels})
}

// emit is best-effort: radar may not be running, and an unconfigured scope stays silent rather than
// sending a label set whose cardinality would panic radar's collector.
func (s metricScope) emit(sender metricSender, message any) {
	if sender == nil || len(s.labels) == 0 {
		return
	}
	_ = sender.Send(radarMetricsProcess, message)
}

// values prefixes the namespace to a metric's remaining label values.
func (s metricScope) values(extra ...string) []string {
	if len(extra) == 0 {
		return s.labels
	}
	return append(append(make([]string, 0, len(s.labels)+len(extra)), s.labels...), extra...)
}

// actorGauges is the controller actor's own state plus its executor tallies, one gauge per field.
type actorGauges struct {
	availability runtime.Availability
	generation   int64
	records      int
	subscribers  int
	executors    int
	drifting     int
}

// publishState reports the actor's gauges. The actor republishes them on its drift-check tick so a
// namespace whose status is steady still reports fresh series.
func (s metricScope) publishState(sender metricSender, state actorGauges) {
	s.set(sender, metricAvailability, availabilityValue(state.availability))
	s.set(sender, metricGeneration, float64(state.generation))
	s.set(sender, metricRecords, float64(state.records))
	s.set(sender, metricSubscribers, float64(state.subscribers))
	s.set(sender, metricExecutors, float64(state.executors))
	s.set(sender, metricExecutorsDrifting, float64(state.drifting))
}

// healthSignal is one namespace's readiness signal on radar. The supervisor owns it rather than the
// actor: it outlives its child, so it can report a crashed controller as unready deliberately
// instead of leaving radar to notice a heartbeat deadline lapse.
//
// It is never unregistered, only held down. Radar reports a node with no signals as healthy, so
// radar.UnregisterService on a drain would make a draining pod read ready.
type healthSignal struct {
	name       gen.Atom
	registered bool
	up         bool
}

func newHealthSignal(namespace string) healthSignal {
	return healthSignal{name: gen.Atom("controller-" + namespace)}
}

// register creates the signal on radar, at most once per successful attempt. Radar marks a freshly
// registered signal up, so the caller must assert its real intent afterwards.
func (h *healthSignal) register(process gen.Process) error {
	if h.registered {
		return nil
	}
	// Readiness only, never liveness: a controller that cannot reach its database should stop
	// serving snapshots, not have k8s kill the pod that owns the only writer.
	if err := radar.RegisterService(process, h.name, radar.ProbeReadiness, healthSignalTimeout); err != nil {
		return err
	}
	h.registered, h.up = true, true
	return nil
}

// apply moves the signal to match serving, sending only on a change: radar's own handlers are
// idempotent, but a resend would log a transition that did not happen.
func (h *healthSignal) apply(process gen.Process, ready bool) {
	if !h.registered || ready == h.up {
		return
	}
	h.up = ready
	if ready {
		_ = radar.ServiceUp(process, h.name)
		return
	}
	_ = radar.ServiceDown(process, h.name)
}

// heartbeat refreshes the deadline while the signal is up. It is deliberately skipped while the
// signal is down, because radar treats a heartbeat as a recovery and would raise it.
func (h *healthSignal) heartbeat(process gen.Process) {
	if !h.registered || !h.up {
		return
	}
	_ = radar.Heartbeat(process, h.name)
}

// readinessSignalState renders the signal for an operator, distinguishing a signal radar never
// accepted from one deliberately held down.
func readinessSignalState(h healthSignal) string {
	switch {
	case !h.registered:
		return "unregistered"
	case h.up:
		return "up"
	default:
		return "down"
	}
}

// availabilityValue maps availability onto a gauge an operator can alert on: below 2 is degraded,
// 0 is a controller serving nothing.
func availabilityValue(availability runtime.Availability) float64 {
	switch availability {
	case runtime.AvailabilityReady:
		return 2
	case runtime.AvailabilityDegraded:
		return 1
	default:
		return 0
	}
}

// supervisorLifecycleValue orders the supervisor lifecycle so a dashboard reads it as progress
// towards a stop.
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

// metricResult labels an outcome for a metric's result label.
func metricResult(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// terminationReason labels an exit by whether it was asked for.
func terminationReason(reason error) string {
	switch reason {
	case gen.TerminateReasonNormal:
		return "normal"
	case gen.TerminateReasonShutdown:
		return "shutdown"
	default:
		return "failure"
	}
}

// elapsedSeconds converts a dispatch timestamp into a histogram sample, reporting zero when the
// write was never timed (a writer replaced mid-flight).
func elapsedSeconds(start time.Time) (float64, bool) {
	if start.IsZero() {
		return 0, false
	}
	return time.Since(start).Seconds(), true
}

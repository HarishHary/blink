package stage

import (
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// Radar metric names by publishing layer; namespace is always a label.

// Kafka reader series.
const (
	metricKafkaReaderAvailability    = "blink_stage_kafka_reader_availability"
	metricKafkaReaderLedgerRecords   = "blink_stage_kafka_reader_ledger_records"
	metricKafkaReaderLedgerBytes     = "blink_stage_kafka_reader_ledger_bytes"
	metricKafkaReaderActive          = "blink_stage_kafka_reader_active"
	metricKafkaReaderDraining        = "blink_stage_kafka_reader_draining"
	metricKafkaReaderRejections      = "blink_stage_kafka_reader_rejections_total"
	metricKafkaReaderFetches         = "blink_stage_kafka_reader_fetches_total"
	metricKafkaReaderFetchDuration   = "blink_stage_kafka_reader_fetch_seconds"
	metricKafkaReaderFetchedRecords  = "blink_stage_kafka_reader_fetched_records_total"
	metricKafkaReaderCommits         = "blink_stage_kafka_reader_commits_total"
	metricKafkaReaderCommitDuration  = "blink_stage_kafka_reader_commit_seconds"
	metricKafkaReaderCommitted       = "blink_stage_kafka_reader_committed_records_total"
	metricKafkaReaderQueue           = "blink_stage_kafka_reader_io_queue"
	metricKafkaReaderQueueRejections = "blink_stage_kafka_reader_io_queue_rejections_total"
)

// Kafka writer series.
const (
	metricKafkaWriterAvailability  = "blink_stage_kafka_writer_availability"
	metricKafkaWriterPending       = "blink_stage_kafka_writer_pending"
	metricKafkaWriterPendingBytes  = "blink_stage_kafka_writer_pending_bytes"
	metricKafkaWriterOperations    = "blink_stage_kafka_writer_operations_total"
	metricKafkaWriterOperationTime = "blink_stage_kafka_writer_operation_seconds"
	metricKafkaWriterRecords       = "blink_stage_kafka_writer_records_total"
	metricKafkaWriterQueueDepth    = "blink_stage_kafka_writer_queue_depth"
	metricKafkaWriterQueueRejects  = "blink_stage_kafka_writer_queue_rejects_total"
)

// Processor supervisor and job-pool series.
const (
	metricProcessorAvailability      = "blink_stage_processor_availability"
	metricProcessorChildren          = "blink_stage_processor_children"
	metricProcessorChildStarts       = "blink_stage_processor_child_starts_total"
	metricProcessorChildTerminations = "blink_stage_processor_child_terminations_total"
	metricJobPoolWorkers             = "blink_stage_job_pool_workers"
	metricJobPoolAvailability        = "blink_stage_job_pool_availability"
)

var (
	stageNamespaceLabels    = []string{"namespace"}
	stageReaderResultLabels = []string{"namespace", "result"}
	stageWriterLabels       = []string{"namespace", "destination"}
	stageWriterResultLabels = []string{"namespace", "destination", "result"}
	stageReaderIOBuckets    = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	stageWriterIOBuckets    = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

	// stageMetricSpecs are registered by the processor supervisor, never by an actor or meta-process.
	stageMetricSpecs = []telemetry.MetricSpec{
		// Kafka reader
		{Kind: telemetry.Gauge, Name: metricKafkaReaderAvailability, Help: "Reader availability: 0 unavailable, 1 degraded, 2 ready", Labels: stageNamespaceLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaReaderLedgerRecords, Help: "Fetched records retained until an acknowledged commit", Labels: stageNamespaceLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaReaderLedgerBytes, Help: "Fetched payload bytes retained in the uncommitted ledger", Labels: stageNamespaceLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaReaderActive, Help: "Reader I/O operation in flight: 0 idle, 1 active", Labels: stageNamespaceLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaReaderDraining, Help: "Reader drain state: 0 running, 1 draining", Labels: stageNamespaceLabels},
		{Kind: telemetry.Counter, Name: metricKafkaReaderRejections, Help: "Reader protocol messages rejected by reason", Labels: []string{"namespace", "reason"}},
		{Kind: telemetry.Counter, Name: metricKafkaReaderFetches, Help: "Reader broker fetch attempts by result", Labels: stageReaderResultLabels},
		{Kind: telemetry.Histogram, Name: metricKafkaReaderFetchDuration, Help: "Seconds spent in one broker fetch attempt", Labels: stageNamespaceLabels, Buckets: stageReaderIOBuckets},
		{Kind: telemetry.Counter, Name: metricKafkaReaderFetchedRecords, Help: "Records returned by broker fetches, including partial failures", Labels: stageNamespaceLabels},
		{Kind: telemetry.Counter, Name: metricKafkaReaderCommits, Help: "Reader broker commit attempts by result", Labels: stageReaderResultLabels},
		{Kind: telemetry.Histogram, Name: metricKafkaReaderCommitDuration, Help: "Seconds spent in one broker commit attempt", Labels: stageNamespaceLabels, Buckets: stageReaderIOBuckets},
		{Kind: telemetry.Counter, Name: metricKafkaReaderCommitted, Help: "Ledger records covered by acknowledged broker commits", Labels: stageNamespaceLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaReaderQueue, Help: "Reader meta I/O commands waiting to run", Labels: stageNamespaceLabels},
		{Kind: telemetry.Counter, Name: metricKafkaReaderQueueRejections, Help: "Reader meta I/O commands rejected because its queue was full", Labels: stageNamespaceLabels},

		// Kafka writer
		{Kind: telemetry.Gauge, Name: metricKafkaWriterAvailability, Help: "Writer availability: 0 unavailable, 1 degraded, 2 ready", Labels: stageWriterLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaWriterPending, Help: "Accepted Kafka publications, including the active write", Labels: stageWriterLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaWriterPendingBytes, Help: "Bytes in accepted Kafka publications, including the active write", Labels: stageWriterLabels},
		{Kind: telemetry.Counter, Name: metricKafkaWriterOperations, Help: "Kafka broker write attempts by result", Labels: stageWriterResultLabels},
		{Kind: telemetry.Histogram, Name: metricKafkaWriterOperationTime, Help: "Seconds spent in one Kafka broker write attempt", Labels: stageWriterLabels, Buckets: stageWriterIOBuckets},
		{Kind: telemetry.Counter, Name: metricKafkaWriterRecords, Help: "Records acknowledged by successful logical Kafka publications", Labels: stageWriterLabels},
		{Kind: telemetry.Gauge, Name: metricKafkaWriterQueueDepth, Help: "Kafka writer meta-process operation queue depth", Labels: stageWriterLabels},
		{Kind: telemetry.Counter, Name: metricKafkaWriterQueueRejects, Help: "Kafka writer admission or meta queue rejections", Labels: stageWriterLabels},

		// Processor supervisor and job pool
		{Kind: telemetry.Gauge, Name: metricProcessorAvailability, Help: "Processing session availability", Labels: stageNamespaceLabels},
		{Kind: telemetry.Gauge, Name: metricProcessorChildren, Help: "Live processing-session children", Labels: stageNamespaceLabels},
		{Kind: telemetry.Counter, Name: metricProcessorChildStarts, Help: "Processing child starts", Labels: []string{"namespace", "child"}},
		{Kind: telemetry.Counter, Name: metricProcessorChildTerminations, Help: "Processing child terminations", Labels: []string{"namespace", "child", "reason"}},
		{Kind: telemetry.Gauge, Name: metricJobPoolWorkers, Help: "Configured job-pool workers", Labels: stageNamespaceLabels},
		{Kind: telemetry.Gauge, Name: metricJobPoolAvailability, Help: "Job-pool availability", Labels: stageNamespaceLabels},
	}
)

// kafkaReaderActorGauges is the reader actor's complete gauge snapshot.
type kafkaReaderActorGauges struct {
	availability  runtime.Availability
	ledgerRecords int
	ledgerBytes   int
	active        bool
	draining      bool
}

// publish sends the reader gauge snapshot.
func (g kafkaReaderActorGauges) publish(labels telemetry.Labels, sender telemetry.Sender) {
	labels.Set(sender, metricKafkaReaderAvailability, telemetry.AvailabilityValue(g.availability))
	labels.Set(sender, metricKafkaReaderLedgerRecords, float64(g.ledgerRecords))
	labels.Set(sender, metricKafkaReaderLedgerBytes, float64(g.ledgerBytes))
	var active, draining float64
	if g.active {
		active = 1
	}
	if g.draining {
		draining = 1
	}
	labels.Set(sender, metricKafkaReaderActive, active)
	labels.Set(sender, metricKafkaReaderDraining, draining)
}

// kafkaWriterActorGauges is the writer actor's complete gauge snapshot.
type kafkaWriterActorGauges struct {
	availability runtime.Availability
	pending      int
	pendingBytes int
}

// publish sends the writer gauge snapshot.
func (g kafkaWriterActorGauges) publish(labels telemetry.Labels, sender telemetry.Sender) {
	labels.Set(sender, metricKafkaWriterAvailability, telemetry.AvailabilityValue(g.availability))
	labels.Set(sender, metricKafkaWriterPending, float64(g.pending))
	labels.Set(sender, metricKafkaWriterPendingBytes, float64(g.pendingBytes))
}

// jobPoolGauges is the job pool's complete gauge snapshot.
type jobPoolGauges struct {
	availability runtime.Availability
	workers      int64
}

// publish sends the job-pool gauge snapshot.
func (g jobPoolGauges) publish(labels telemetry.Labels, sender telemetry.Sender) {
	labels.Set(sender, metricJobPoolWorkers, float64(g.workers))
	labels.Set(sender, metricJobPoolAvailability, telemetry.AvailabilityValue(g.availability))
}

// processorSupervisorGauges is the processor supervisor's complete gauge snapshot.
type processorSupervisorGauges struct {
	availability runtime.Availability
	children     int
}

// publish sends the processor-supervisor gauge snapshot.
func (g processorSupervisorGauges) publish(labels telemetry.Labels, sender telemetry.Sender) {
	labels.Set(sender, metricProcessorAvailability, telemetry.AvailabilityValue(g.availability))
	labels.Set(sender, metricProcessorChildren, float64(g.children))
}

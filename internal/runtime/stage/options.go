package stage

import (
	"fmt"
	"time"

	"ergo.services/ergo/gen"
)

// KafkaReaderActorOptions configures one Kafka reader actor.
type KafkaReaderActorOptions struct {
	Namespace       string
	MaxRecords      int
	MaxPendingBytes int
	FetchTimeout    time.Duration
	CommitTimeout   time.Duration
	RetryMin        time.Duration
	RetryMax        time.Duration
	RestartMin      time.Duration
	RestartMax      time.Duration
}

// KafkaWriterActorOptions configures one Kafka writer actor destination.
type KafkaWriterActorOptions struct {
	Namespace    string
	Destination  string
	MaxPending   int
	MaxRecords   int
	MaxBytes     int
	WriteTimeout time.Duration
	RetryMin     time.Duration
	RetryMax     time.Duration
	RestartMin   time.Duration
	RestartMax   time.Duration
}

// JobPoolOptions configures the independent-job worker pool.
type JobPoolOptions struct {
	Namespace         string
	Workers           int64
	MailboxSize       int64
	WorkerMailboxSize int64
	WorkerFactory     gen.ProcessFactory
	WorkerArgs        []any
}

// ProcessorSupervisorOptions configures the processor supervisor subtree.
type ProcessorSupervisorOptions struct {
	Namespace        string
	Reader           KafkaReaderActorOptions
	Writer           KafkaWriterActorOptions
	DLQWriter        KafkaWriterActorOptions
	Coordinator      gen.ProcessFactory
	JobPool          JobPoolOptions
	MailboxSize      int64
	RestartIntensity uint16
	RestartPeriod    uint16
}

// validateKafkaReaderOptions validates Kafka reader actor options.
func validateKafkaReaderOptions(opts KafkaReaderActorOptions) error {
	if opts.Namespace == "" || opts.MaxRecords <= 0 || opts.MaxPendingBytes <= 0 || opts.FetchTimeout <= 0 || opts.CommitTimeout <= 0 {
		return fmt.Errorf("kafka reader: namespace, positive record and pending-byte limits, and positive timeouts are required")
	}
	return nil
}

// validateKafkaWriterOptions validates Kafka writer actor options.
func validateKafkaWriterOptions(opts KafkaWriterActorOptions) error {
	if opts.Namespace == "" || opts.Destination == "" {
		return fmt.Errorf("kafka writer: namespace and destination are required")
	}
	if opts.MaxPending <= 0 || opts.MaxRecords <= 0 || opts.MaxBytes <= 0 || opts.WriteTimeout <= 0 {
		return fmt.Errorf("kafka writer: positive pending, record, byte, and write-timeout limits are required")
	}
	if opts.RetryMin <= 0 || opts.RetryMax < opts.RetryMin {
		return fmt.Errorf("kafka writer: positive retry minimum and bounded retry maximum are required")
	}
	return nil
}

// validateJobPoolOptions validates job pool options.
func validateJobPoolOptions(opts JobPoolOptions) error {
	if opts.Namespace == "" || opts.WorkerFactory == nil {
		return fmt.Errorf("job pool: namespace and worker factory are required")
	}
	if opts.Workers <= 0 || opts.MailboxSize <= 0 || opts.WorkerMailboxSize <= 0 {
		return fmt.Errorf("job pool: positive worker count and mailbox sizes are required")
	}
	return nil
}

// validateProcessorSupervisorOptions validates processor supervisor options.
func validateProcessorSupervisorOptions(opts ProcessorSupervisorOptions) error {
	if opts.Namespace == "" || opts.Coordinator == nil {
		return fmt.Errorf("processor supervisor: namespace and coordinator factory are required")
	}
	if opts.MailboxSize <= 0 {
		return fmt.Errorf("processor supervisor: mailbox size must be positive")
	}
	reader := opts.Reader
	reader.Namespace = opts.Namespace
	if err := validateKafkaReaderOptions(reader); err != nil {
		return err
	}
	for _, configuredWriter := range []struct {
		role string
		opts KafkaWriterActorOptions
	}{{"output", opts.Writer}, {"dlq", opts.DLQWriter}} {
		writer := configuredWriter.opts
		writer.Namespace = opts.Namespace
		if err := validateKafkaWriterOptions(writer); err != nil {
			return err
		}
		if writer.Destination != configuredWriter.role {
			return fmt.Errorf("processor supervisor: %s writer destination must be %q", configuredWriter.role, configuredWriter.role)
		}
	}
	pool := opts.JobPool
	pool.Namespace = opts.Namespace
	return validateJobPoolOptions(pool)
}

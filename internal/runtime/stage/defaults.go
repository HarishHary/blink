package stage

import (
	"time"

	"ergo.services/ergo/gen"
)

const (
	defaultKafkaRestartMin                  = 100 * time.Millisecond
	defaultKafkaRestartMax                  = 5 * time.Second
	defaultKafkaReaderRetryMin              = 100 * time.Millisecond
	defaultKafkaReaderRetryMax              = 5 * time.Second
	kafkaReaderRetryAttemptBudget           = 5
	kafkaReaderUnavailableThreshold         = kafkaReaderRetryAttemptBudget
	defaultKafkaWriterRetryMin              = 100 * time.Millisecond
	defaultKafkaWriterRetryMax              = 5 * time.Second
	kafkaWriterRetryAttemptBudget           = 5
	kafkaWriterUnavailableThreshold         = kafkaWriterRetryAttemptBudget
	defaultProcessorRestartIntensity uint16 = 5
	defaultProcessorRestartPeriod    uint16 = 10
)

// kafkaReaderActorOptionsWithDefaults applies reader retry and restart defaults.
func kafkaReaderActorOptionsWithDefaults(opts KafkaReaderActorOptions) KafkaReaderActorOptions {
	if opts.RestartMin <= 0 {
		opts.RestartMin = defaultKafkaRestartMin
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = defaultKafkaRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = defaultKafkaReaderRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = defaultKafkaReaderRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

// kafkaWriterActorOptionsWithDefaults applies writer retry and restart defaults.
func kafkaWriterActorOptionsWithDefaults(opts KafkaWriterActorOptions) KafkaWriterActorOptions {
	if opts.RestartMin <= 0 {
		opts.RestartMin = defaultKafkaRestartMin
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = defaultKafkaRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = defaultKafkaWriterRetryMin
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = defaultKafkaWriterRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

// processorSupervisorOptionsWithDefaults applies processor supervisor defaults.
func processorSupervisorOptionsWithDefaults(opts ProcessorSupervisorOptions) ProcessorSupervisorOptions {
	opts.Reader = kafkaReaderActorOptionsWithDefaults(opts.Reader)
	for i := range opts.Writers {
		opts.Writers[i] = kafkaWriterActorOptionsWithDefaults(opts.Writers[i])
	}
	if opts.RestartIntensity == 0 {
		opts.RestartIntensity = defaultProcessorRestartIntensity
	}
	if opts.RestartPeriod == 0 {
		opts.RestartPeriod = defaultProcessorRestartPeriod
	}
	return opts
}

// ProcessorSupervisorName returns the processor supervisor name for a namespace.
func ProcessorSupervisorName(namespace string) gen.Atom {
	return subtreeName(namespace, "supervisor")
}

// KafkaReaderActorName returns the Kafka reader actor name for a namespace.
func KafkaReaderActorName(namespace string) gen.Atom { return subtreeName(namespace, "reader") }

// KafkaWriterActorName returns the Kafka writer actor name for a destination.
func KafkaWriterActorName(namespace, destination string) gen.Atom {
	return subtreeName(namespace, "writer-"+destination)
}

// CoordinatorName returns the coordinator name for a namespace.
func CoordinatorName(namespace string) gen.Atom { return subtreeName(namespace, "coordinator") }

// JobPoolName returns the job pool name for a namespace.
func JobPoolName(namespace string) gen.Atom { return subtreeName(namespace, "job-pool") }

// subtreeName returns a stage processor subtree name.
func subtreeName(namespace, suffix string) gen.Atom {
	return gen.Atom("stage-" + namespace + "-processor-" + suffix)
}

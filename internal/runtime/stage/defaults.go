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
	defaultKafkaWriterRetryMin              = 100 * time.Millisecond
	defaultKafkaWriterRetryMax              = 5 * time.Second
	kafkaWriterRetryAttemptBudget           = 5
	defaultProcessorRestartIntensity uint16 = 5
	defaultProcessorRestartPeriod    uint16 = 10
)

func kafkaReaderActorOptionsWithDefaults(opts KafkaReaderActorOptions) KafkaReaderActorOptions {
	if opts.RestartMin <= 0 {
		opts.RestartMin = defaultKafkaRestartMin
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = defaultKafkaRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = defaultKafkaReaderRetryMin
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = defaultKafkaReaderRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

func kafkaWriterActorOptionsWithDefaults(opts KafkaWriterActorOptions) KafkaWriterActorOptions {
	if opts.RestartMin <= 0 {
		opts.RestartMin = defaultKafkaRestartMin
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = defaultKafkaRestartMax
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = defaultKafkaWriterRetryMin
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = defaultKafkaWriterRetryMax
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = opts.RetryMin
	}
	return opts
}

func processorSupervisorOptionsWithDefaults(opts ProcessorSupervisorOptions) ProcessorSupervisorOptions {
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

func ProcessorSupervisorName(namespace string) gen.Atom {
	return subtreeName(namespace, "supervisor")
}
func KafkaReaderActorName(namespace string) gen.Atom { return subtreeName(namespace, "reader") }
func KafkaWriterActorName(namespace, destination string) gen.Atom {
	return subtreeName(namespace, "writer-"+destination)
}
func CoordinatorName(namespace string) gen.Atom { return subtreeName(namespace, "coordinator") }
func JobPoolName(namespace string) gen.Atom     { return subtreeName(namespace, "job-pool") }
func subtreeName(namespace, suffix string) gen.Atom {
	return gen.Atom("stage-" + namespace + "-processor-" + suffix)
}

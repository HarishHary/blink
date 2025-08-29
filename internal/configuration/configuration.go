package configuration

import "fmt"

type ServiceConfiguration struct {
	Name        string `env:"APP_NAME"`
	Environment string `env:"APP_STACK_ENVIRONMENT"`

	Tenant string `env:"TENANT"`
	Domain string `env:"DOMAIN"`

	Token       string `file:"/var/run/secrets/kubernetes.io/serviceaccount/token"`
	Certificate string `file:"/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"`

	Kubernetes Kubernetes
	JWT        JWT
	Kafka      KafkaConfig
	Topics     KafkaTopicsGroups
	Executor   ExecutorConfig
}

// ServiceRole returns the role used by the service to perform operations
func (configuration *ServiceConfiguration) ServiceRole() string {
	return fmt.Sprintf("%s-%s-admin", configuration.Name, configuration.Tenant)
}

type JWT struct {
	Product  string `env:"PRODUCT"`
	Audience string `env:"AUTH0_AUDIENCE"`
	Issuer   string `env:"AUTH0_ISSUER"`
}

type Kubernetes struct {
	Target string `env:"KUBERNETES_SERVICE_HOST"`
	Port   uint   `env:"KUBERNETES_SERVICE_PORT"`
}

// KafkaConfig holds Kafka bootstrap addresses for the event-driven pipeline.
type KafkaConfig struct {
	// Brokers is a comma-separated list of Kafka bootstrap servers (e.g. "kafka:9092,other:9092").
	Brokers string `env:"KAFKA_BROKERS"`
}

// KafkaTopicsGroups defines Kafka topic and consumer-group names for each pipeline stage.
// These environment variables drive the event-driven pipeline skeleton services.
type KafkaTopicsGroups struct {
	MatcherTopic    string `env:"KAFKA_TOPIC_MATCHER"`
	MatcherGroup    string `env:"KAFKA_GROUP_MATCHEr"`
	ExecTopic       string `env:"KAFKA_TOPIC_EXEC"`
	ExecGroup       string `env:"KAFKA_GROUP_EXEC"`
	TunerTopic      string `env:"KAFKA_TOPIC_TUNER"`
	TunerGroup      string `env:"KAFKA_GROUP_TUNER"`
	EnricherTopic   string `env:"KAFKA_TOPIC_ENRICHER"`
	EnricherGroup   string `env:"KAFKA_GROUP_ENRICHER"`
	FormatterTopic  string `env:"KAFKA_TOPIC_FORMATTER"`
	FormattterGroup string `env:"KAFKA_GROUP_FORMATTER"`
	DispatcherTopic string `env:"KAFKA_TOPIC_DISPATCHER"`
	DispatcherGroup string `env:"KAFKA_GROUP_DISPATCHER"`
}

// ExecutorConfig holds batch/concurrency/timeout settings for rule executor.
type ExecutorConfig struct {
	// BatchSize is the maximum number of events to read in one batch.
	BatchSize int `env:"EXECUTOR_BATCH_SIZE,optional"`
	// Concurrency is the max number of parallel rule evaluations.
	Concurrency int `env:"EXECUTOR_CONCURRENCY,optional"`
	// TimeoutSec is the per-event evaluation timeout in seconds.
	TimeoutSec int `env:"EXECUTOR_TIMEOUT_SEC,optional"`
}

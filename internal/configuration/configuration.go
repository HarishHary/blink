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
	Merger     MergerConfig
}

// ServiceRole returns the role used by the service to perform operations
func (configuration *ServiceConfiguration) ServiceRole() string {
	return fmt.Sprintf("%s-%s-admin", configuration.Name, configuration.Tenant)
}

func (c *ServiceConfiguration) Validate() error {
	if c.Kubernetes.Port == 0 {
		return fmt.Errorf("KUBERNETES_SERVICE_PORT must be > 0")
	}
	return nil
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

type KafkaConfig struct {
	// Brokers is a comma-separated list of Kafka bootstrap servers (e.g. "kafka:9092,other:9092").
	Brokers string `env:"KAFKA_BROKERS"`
}

// KafkaTopicsGroups defines Kafka topic and consumer-group names for each pipeline stage.
// These environment variables drive the event-driven pipeline skeleton services.
type KafkaTopicsGroups struct {
	MatcherTopic      string `env:"KAFKA_TOPIC_MATCHER"`
	MatcherGroup      string `env:"KAFKA_GROUP_MATCHER"`
	ExecTopic         string `env:"KAFKA_TOPIC_EXEC"`
	ExecGroup         string `env:"KAFKA_GROUP_EXEC"`
	MergerTopic       string `env:"KAFKA_TOPIC_MERGER"`
	MergerGroup       string `env:"KAFKA_GROUP_MERGER"`
	TunerTopic        string `env:"KAFKA_TOPIC_TUNER"`
	TunerGroup        string `env:"KAFKA_GROUP_TUNER"`
	TunerDLQTopic     string `env:"KAFKA_TOPIC_TUNER_DLQ,optional"`
	EnricherTopic     string `env:"KAFKA_TOPIC_ENRICHER"`
	EnricherGroup     string `env:"KAFKA_GROUP_ENRICHER"`
	EnricherDLQTopic  string `env:"KAFKA_TOPIC_ENRICHER_DLQ,optional"`
	FormatterTopic    string `env:"KAFKA_TOPIC_FORMATTER"`
	FormatterGroup    string `env:"KAFKA_GROUP_FORMATTER"`
	FormatterDLQTopic string `env:"KAFKA_TOPIC_FORMATTER_DLQ,optional"`
	DispatcherTopic   string `env:"KAFKA_TOPIC_DISPATCHER"`
	DispatcherGroup   string `env:"KAFKA_GROUP_DISPATCHER"`
}

type ExecutorConfig struct {
	// BatchSize is the maximum number of events to read in one batch.
	BatchSize int `env:"EXECUTOR_BATCH_SIZE,optional"`
	// Concurrency is the max number of parallel rule evaluations.
	Concurrency int `env:"EXECUTOR_CONCURRENCY,optional"`
	// TimeoutSec is the per-event evaluation timeout in seconds.
	TimeoutSec int `env:"EXECUTOR_TIMEOUT_SEC,optional"`
}

type MergerConfig struct {
	// MaxGroups caps the number of live merge groups held in memory per replica.
	// When the cap is exceeded the oldest group (earliest expiry) is flushed
	// immediately rather than waiting for its window to close.
	// 0 means unlimited — only safe when merge_by_keys have low cardinality.
	MaxGroups int `env:"MERGER_MAX_GROUPS,optional"`
}

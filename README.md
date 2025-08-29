Detection engine using Apache Beam, Apache Flink and Kubernetes.

## Event-driven pipeline with Kafka

Blink now supports a fully event-driven pipeline using Apache Kafka as the broker. Each stage (rule-engine, alert-engine, alert-processor) runs as a standalone microservice and exchanges messages via Kafka topics.

Configuration is environment-driven; see DEVELOPMENT.md for details on setting up Kafka topics, consumer groups, and example Kubernetes manifests.
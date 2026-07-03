package services

import "github.com/harishhary/blink/internal/brokers"

// Common holds infrastructure config that every service needs. Each service's own config
// struct (defined in its cmd/<service> package) embeds Common and adds only the fields that
// service uses - so each binary loads, and requires, exactly its own dependencies. Grow
// Common as services reveal genuinely shared needs.
type Common struct {
	Kafka brokers.KafkaConfig
}

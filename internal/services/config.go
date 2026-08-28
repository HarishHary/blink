package services

import "github.com/harishhary/blink/internal/brokers"

// Common holds infrastructure config that every service needs. Each service's own config
// struct (defined in its cmd/<service> package) embeds Common and adds only the fields that
// service uses - so each binary loads, and requires, exactly its own dependencies. Grow
// Common as services reveal genuinely shared needs.
type Common struct {
	Kafka brokers.KafkaConfig
	// Env selects log verbosity (dev|staging|integration|prod); see logger.New. Optional: an
	// empty value defaults to integration (Debug off), which is the prod-safe default.
	Env string `env:"ENVIRONMENT,optional"`
	// Observer independently enables Ergo's observer app (see NodeOptions.Observer) - Env == "dev"
	// still implies it, so a prod-level Env can opt in without dropping to debug logging.
	Observer bool `env:"OBSERVER_ENABLED,optional"`
	// RadarHost and RadarPort bind the node's radar endpoint, which serves Prometheus metrics and
	// readiness signals; empty values keep plugin.RadarOptions' pod-reachable defaults.
	RadarHost string `env:"RADAR_HOST,optional"`
	RadarPort uint16 `env:"RADAR_PORT,optional"`
}

package services

import "github.com/harishhary/blink/internal/brokers"

// Common holds infrastructure config that every service needs. Each service's own config
// struct (defined in its cmd/<service> package) embeds Common and adds only the fields that
// service uses - so each binary loads, and requires, exactly its own dependencies. Grow
// Common as services reveal genuinely shared needs.
type Common struct {
	Kafka           brokers.KafkaConfig
	Env             string `env:"ENVIRONMENT,optional"`   // Env names the Ergo cluster this process joins, as blink-<env>; it gates nothing else.
	Debug           bool   `env:"DEBUG,optional"`         // Debug raises this process's logger and its Ergo node to debug level.
	RadarEnabled    bool   `env:"RADAR_ENABLED,optional"` // radar serves Prometheus metrics and readiness signals
	RadarHost       string `env:"RADAR_HOST,optional"`
	RadarPort       uint16 `env:"RADAR_PORT,optional"`
	ObserverEnabled bool   `env:"OBSERVER_ENABLED,optional"` // observer is Ergo's process-inspection UI
	ObserverHost    string `env:"OBSERVER_HOST,optional"`
	ObserverPort    uint16 `env:"OBSERVER_PORT,optional"`
	MCPEnabled      bool   `env:"MCP_ENABLED,optional"` // MCP is Ergo's agent server.
	MCPHost         string `env:"MCP_HOST,optional"`
	MCPPort         uint16 `env:"MCP_PORT,optional"`
}

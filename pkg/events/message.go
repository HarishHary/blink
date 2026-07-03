package events

import "github.com/harishhary/blink/internal/messaging"

// EventMessage is the bus message carrying one Event through the pipeline.
type EventMessage struct {
	messaging.Message
	Event Event
}

package events

import "github.com/harishhary/blink/internal/messaging"

type EventMessage struct {
	messaging.Message
	Event Event
}

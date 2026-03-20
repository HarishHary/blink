package messaging

// Message is the interface implemented by all plugin lifecycle event types.
type Message interface {
	ismessage()
}

type IsMessage struct{}

func (IsMessage) ismessage() {}

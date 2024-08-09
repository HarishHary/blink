package messaging

import (
	"sync"
)

type ServiceName uint

const (
	QueueSize = 1000
)

type MessageBroadcast struct {
	blocking bool
	send     chan<- Message
}

type MessageQueue <-chan Message

type Messages struct {
	lock sync.Locker

	subscribers map[ServiceName][]MessageBroadcast
}

func New() *Messages {
	return &Messages{lock: &sync.Mutex{}, subscribers: make(map[ServiceName][]MessageBroadcast)}
}

func (messages *Messages) Subscribe(name ServiceName, blocking bool) MessageQueue {
	messages.lock.Lock()
	defer messages.lock.Unlock()

	queue := make(chan Message, QueueSize)
	if _, exists := messages.subscribers[name]; !exists {
		messages.subscribers[name] = make([]MessageBroadcast, 0)
	}

	messages.subscribers[name] = append(messages.subscribers[name], MessageBroadcast{
		blocking: blocking,
		send:     queue,
	})
	return MessageQueue(queue)
}

func (messages *Messages) Publish(from ServiceName, message Message) {
	for _, queue := range messages.subscribers[from] {
		if queue.blocking || len(queue.send) < QueueSize {
			queue.send <- message
		}
	}
}

type Message interface {
	ismessage()
}

type IsMessage struct{}

func (IsMessage) ismessage() {}

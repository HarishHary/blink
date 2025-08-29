package broker

import (
	"context"
)

// Message is a broker-level envelope for a key/value payload.
type Message struct {
	Topic     string
	Partition int
	Offset    int64
	Key       []byte
	Value     []byte
}

// Reader provides a stream of messages from a topic.
type Reader interface {
	// ReadMessage reads the next message from the broker.
	ReadMessage(ctx context.Context) (Message, error)
	// ReadBatch reads up to batchSize messages from the broker.
	ReadBatch(ctx context.Context, batchSize int) ([]Message, error)
	// CommitMessages commits offsets for messages that have been processed.
	CommitMessages(ctx context.Context, msgs ...Message) error
	// Close frees any resources held by the reader.
	Close() error
}

// Writer publishes messages to a topic.
type Writer interface {
	// WriteMessages writes one or more messages to the broker.
	WriteMessages(ctx context.Context, msgs ...Message) error
	// Close frees any resources held by the writer.
	Close() error
}

// Broker constructs Readers and Writers for Kafka (or other implementations).
type Broker interface {
	// NewReader returns a Reader for the given topic and consumer group.
	NewReader(topic, groupID string) Reader
	// NewWriter returns a Writer for the given topic.
	NewWriter(topic string) Writer
}

package brokers

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

// Reader consumes messages without implicitly committing offsets.
type Reader interface {
	// ReadMessage reads the next message from the broker without committing its offset.
	ReadMessage(ctx context.Context) (Message, error)
	// ReadBatch reads up to batchSize messages without committing offsets.
	ReadBatch(ctx context.Context, batchSize int) ([]Message, error)
	// ReadLag returns the distance between the reader's current position and the log end.
	ReadLag(ctx context.Context) (int64, error)
	// CommitMessages commits offsets for processed consumer-group messages.
	CommitMessages(ctx context.Context, msgs ...Message) error
	// Close frees any resources held by the reader.
	Close() error
}

// Writer publishes messages to a topic.
type Writer interface {
	// WriteMessages synchronously writes and acknowledges one or more messages.
	WriteMessages(ctx context.Context, msgs ...Message) error
	// Close frees any resources held by the writer.
	Close() error
}

// Broker constructs Readers and Writers for Kafka (or other implementations).
type Broker interface {
	// NewReader returns a consumer-group Reader for the given topic.
	NewReader(topic, groupID string) Reader
	// NewBroadcastReader returns an independent Reader for a topic's full stream.
	NewBroadcastReader(topic string) Reader
	// NewWriter returns a Writer for the given topic.
	NewWriter(topic string) Writer
}

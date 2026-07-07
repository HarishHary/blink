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

// Reader provides a stream of messages from a topic.
type Reader interface {
	// ReadMessage reads the next message from the broker.
	ReadMessage(ctx context.Context) (Message, error)
	// ReadBatch reads up to batchSize messages from the broker.
	ReadBatch(ctx context.Context, batchSize int) ([]Message, error)
	// Lag returns how many messages lie between the reader's current position and the
	// log end (high-water mark): 0 means the reader has consumed everything currently
	// published. A broadcast consumer uses this to detect cold-start catch-up - compaction
	// bounds how much it must replay, but only lag tells it when it has reached the end.
	// Implementations that cannot report lag return 0 (treated as "caught up").
	Lag(ctx context.Context) (int64, error)
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
	// NewReader returns a Reader for the given topic and consumer group. Consumer-group
	// semantics: partitions are split across the group's members (work queue) - each
	// message is delivered to exactly one member of the group.
	NewReader(topic, groupID string) Reader
	// NewBroadcastReader returns a Reader that consumes a topic's full stream
	// independently of any other consumer - every reader gets every message. Use it for
	// broadcast control-plane topics (e.g. controller snapshots) where each pod must see
	// every message, not a partition subset. The topic must be single-partition (a
	// broadcast gains nothing from partitioning) and should be log-compacted so a
	// freshly-started reader converges to the latest message instead of replaying history.
	NewBroadcastReader(topic string) Reader
	// NewWriter returns a Writer for the given topic.
	NewWriter(topic string) Writer
}

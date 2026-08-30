package brokers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	kafkaBatchLinger = 100 * time.Millisecond
	kafkaIOTimeout   = 5 * time.Second
	kafkaMaxAttempts = 3
)

// KafkaConfig configures a Kafka Broker.
type KafkaConfig struct {
	// Brokers is a comma-separated list of Kafka bootstrap servers (e.g. "kafka:9092,other:9092").
	Brokers string `env:"KAFKA_BROKERS"`
}

// kafkaBroker is the default Kafka-based Broker implementation.
type kafkaBroker struct {
	brokers     []string
	dialTimeout time.Duration
}

// NewKafkaBroker constructs a Broker using the given KafkaConfig.
func NewKafkaBroker(cfg KafkaConfig) Broker {
	return &kafkaBroker{
		brokers:     strings.Split(cfg.Brokers, ","),
		dialTimeout: 10 * time.Second,
	}
}

// NewReader returns a kafka-backed Reader for the specified topic and group.
func (kb *kafkaBroker) NewReader(topic, groupID string) Reader {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        kb.brokers,
		Topic:          topic,
		GroupID:        groupID,
		MinBytes:       1,
		MaxBytes:       10e6, // 10MB
		CommitInterval: 0,
		Dialer:         &kafka.Dialer{Timeout: kb.dialTimeout},
	})
	return &kafkaReader{reader: r}
}

// NewBroadcastReader returns an independent reader for partition 0 from its earliest offset.
func (kb *kafkaBroker) NewBroadcastReader(topic string) Reader {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: kb.brokers,
		Topic:   topic,
		// Kafka defaults provide group-less partition-0 reads from the earliest offset.
		MinBytes: 1,
		MaxBytes: 10e6, // 10MB
		Dialer:   &kafka.Dialer{Timeout: kb.dialTimeout},
	})
	return &kafkaReader{reader: r}
}

// NewWriter returns a kafka-backed Writer for the specified topic.
func (kb *kafkaBroker) NewWriter(topic string) Writer {
	w := kafka.NewWriter(kafka.WriterConfig{
		Brokers: kb.brokers,
		Topic:   topic,
		// Hash keeps equal keys on one partition; nil keys fall back to round-robin.
		Balancer:     &kafka.Hash{},
		Dialer:       &kafka.Dialer{Timeout: kb.dialTimeout},
		MaxAttempts:  kafkaMaxAttempts,
		BatchTimeout: kafkaBatchLinger,
		RequiredAcks: int(kafka.RequireAll),
		Async:        false,
	})
	return &kafkaWriter{writer: w}
}

// kafkaReaderAdapter enables explicit fetch and commit semantics without a Kafka broker in tests.
type kafkaReaderAdapter interface {
	FetchMessage(context.Context) (kafka.Message, error)
	CommitMessages(context.Context, ...kafka.Message) error
	ReadLag(context.Context) (int64, error)
	Close() error
}

// kafkaReader wraps kafka.Reader to implement Reader.
type kafkaReader struct {
	reader kafkaReaderAdapter
}

// ReadMessage fetches the next message from Kafka without committing it.
func (rd *kafkaReader) ReadMessage(ctx context.Context) (Message, error) {
	m, err := rd.reader.FetchMessage(ctx)
	if err != nil {
		return Message{}, err
	}
	return Message{
		Topic:     m.Topic,
		Partition: m.Partition,
		Offset:    m.Offset,
		Key:       m.Key,
		Value:     m.Value,
	}, nil
}

// ReadBatch fetches up to batchSize uncommitted messages with a bounded partial-batch linger.
func (rd *kafkaReader) ReadBatch(ctx context.Context, batchSize int) ([]Message, error) {
	if batchSize <= 0 {
		return nil, fmt.Errorf("kafka: batch size must be positive: %d", batchSize)
	}

	m, err := rd.reader.FetchMessage(ctx)
	if err != nil {
		return nil, err
	}
	out := []Message{{
		Topic:     m.Topic,
		Partition: m.Partition,
		Offset:    m.Offset,
		Key:       m.Key,
		Value:     m.Value,
	}}
	if batchSize == 1 {
		return out, nil
	}

	lingerCtx, cancel := context.WithTimeout(ctx, kafkaBatchLinger)
	defer cancel()
	for len(out) < batchSize {
		m, err = rd.reader.FetchMessage(lingerCtx)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return out, nil
		}
		out = append(out, Message{
			Topic:     m.Topic,
			Partition: m.Partition,
			Offset:    m.Offset,
			Key:       m.Key,
			Value:     m.Value,
		})
	}
	return out, nil
}

// ReadLag returns the distance from this reader's position to Kafka's high-water mark.
func (rd *kafkaReader) ReadLag(ctx context.Context) (int64, error) {
	return rd.reader.ReadLag(ctx)
}

// CommitMessages commits offsets of the processed messages.
func (rd *kafkaReader) CommitMessages(ctx context.Context, msgs ...Message) error {
	var written []kafka.Message
	for _, m := range msgs {
		written = append(written, kafka.Message{Topic: m.Topic, Partition: m.Partition, Offset: m.Offset})
	}
	return rd.reader.CommitMessages(ctx, written...)
}

// Close closes the underlying kafka.Reader.
func (rd *kafkaReader) Close() error { return rd.reader.Close() }

// kafkaWriterAdapter enables writes without a Kafka broker in tests.
type kafkaWriterAdapter interface {
	WriteMessages(context.Context, ...kafka.Message) error
	Close() error
}

// kafkaWriter wraps kafka.Writer to implement Writer.
type kafkaWriter struct {
	writer kafkaWriterAdapter
}

// WriteMessages writes one or more messages to Kafka.
func (wr *kafkaWriter) WriteMessages(ctx context.Context, msgs ...Message) error {
	ctx, cancel := context.WithTimeout(ctx, kafkaIOTimeout)
	defer cancel()
	var records []kafka.Message
	for _, m := range msgs {
		records = append(records, kafka.Message{Key: m.Key, Value: m.Value})
	}
	return wr.writer.WriteMessages(ctx, records...)
}

// Close closes the underlying kafka.Writer.
func (wr *kafkaWriter) Close() error { return wr.writer.Close() }

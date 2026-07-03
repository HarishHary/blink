package brokers

import (
	"context"
	"strings"
	"time"

	"github.com/harishhary/blink/internal/configuration"
	"github.com/segmentio/kafka-go"
)

// kafkaBroker is the default Kafka-based Broker implementation.
type kafkaBroker struct {
	brokers     []string
	dialTimeout time.Duration
}

// NewKafkaBroker constructs a Broker using the given KafkaConfig.
func NewKafkaBroker(cfg configuration.KafkaConfig) Broker {
	return &kafkaBroker{
		brokers:     strings.Split(cfg.Brokers, ","),
		dialTimeout: 10 * time.Second,
	}
}

// NewReader returns a kafka-backed Reader for the specified topic and group.
func (kb *kafkaBroker) NewReader(topic, groupID string) Reader {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  kb.brokers,
		Topic:    topic,
		GroupID:  groupID,
		MinBytes: 10e3, // 10KB
		MaxBytes: 10e6, // 10MB
		Dialer:   &kafka.Dialer{Timeout: kb.dialTimeout},
	})
	return &kafkaReader{r: r}
}

// NewWriter returns a kafka-backed Writer for the specified topic.
func (kb *kafkaBroker) NewWriter(topic string) Writer {
	w := kafka.NewWriter(kafka.WriterConfig{
		Brokers:  kb.brokers,
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
		Dialer:   &kafka.Dialer{Timeout: kb.dialTimeout},
	})
	return &kafkaWriter{w: w}
}

// kafkaReader wraps kafka.Reader to implement Reader.
type kafkaReader struct {
	r *kafka.Reader
}

// ReadMessage reads the next message from Kafka.
func (rd *kafkaReader) ReadMessage(ctx context.Context) (Message, error) {
	m, err := rd.r.ReadMessage(ctx)
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

// ReadBatch reads up to batchSize messages from Kafka.
func (rd *kafkaReader) ReadBatch(ctx context.Context, batchSize int) ([]Message, error) {
	var out []Message
	for i := 0; i < batchSize; i++ {
		m, err := rd.r.ReadMessage(ctx)
		if err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
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

// CommitMessages commits offsets of the processed messages.
func (rd *kafkaReader) CommitMessages(ctx context.Context, msgs ...Message) error {
	var written []kafka.Message
	for _, m := range msgs {
		written = append(written, kafka.Message{Topic: m.Topic, Partition: m.Partition, Offset: m.Offset + 1})
	}
	return rd.r.CommitMessages(ctx, written...)
}

// Close closes the underlying kafka.Reader.
func (rd *kafkaReader) Close() error { return rd.r.Close() }

// kafkaWriter wraps kafka.Writer to implement Writer.
type kafkaWriter struct {
	w *kafka.Writer
}

// WriteMessages writes one or more messages to Kafka.
func (wr *kafkaWriter) WriteMessages(ctx context.Context, msgs ...Message) error {
	var records []kafka.Message
	for _, m := range msgs {
		records = append(records, kafka.Message{Key: m.Key, Value: m.Value})
	}
	return wr.w.WriteMessages(ctx, records...)
}

// Close closes the underlying kafka.Writer.
func (wr *kafkaWriter) Close() error { return wr.w.Close() }

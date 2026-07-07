package brokers

import (
	"context"
	"fmt"

	azeventhubs "github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/v2"
)

// Config holds the connection-level Event Hubs credentials.
// EventHubName (topic) and ConsumerGroup are passed per Reader/Writer to match
// the Broker interface; Partition applies to Readers only.
type Config struct {
	Hostname  string `env:"SVC_EVENTHUB_HOST"`
	Username  string `env:"SVC_EVENTHUB_USERNAME"`
	Password  string `env:"SVC_EVENTHUB_PASSWORD"`
	Partition string // partition ID a Reader consumes from
}

func (c Config) connectionString(eventHubName string) string {
	return fmt.Sprintf(
		"Endpoint=sb://%s/;SharedAccessKeyName=%s;SharedAccessKey=%s;EntityPath=%s",
		c.Hostname, c.Username, c.Password, eventHubName,
	)
}

// eventHubBroker is the Event Hubs Broker implementation.
type eventHubBroker struct {
	cfg Config
}

// NewEventHubBroker constructs a Broker from connection-level Config.
func NewEventHubBroker(cfg Config) Broker {
	return &eventHubBroker{cfg: cfg}
}

// NewReader returns a Reader bound to the given Event Hub (topic) and consumer group.
func (b *eventHubBroker) NewReader(topic, groupID string) Reader {
	return &eventHubReader{cfg: b.cfg, eventHubName: topic, consumerGroup: groupID}
}

// NewBroadcastReader reads via the default consumer group. In Event Hubs a consumer
// group is already an independent view of the full stream, so this is inherently a
// broadcast read - every pod's reader sees every event. (eventHubBroker is currently
// unwired; this satisfies the Broker interface.)
func (b *eventHubBroker) NewBroadcastReader(topic string) Reader {
	return &eventHubReader{cfg: b.cfg, eventHubName: topic, consumerGroup: "$Default"}
}

// NewWriter returns a Writer bound to the given Event Hub (topic).
func (b *eventHubBroker) NewWriter(topic string) Writer {
	return &eventHubWriter{cfg: b.cfg, eventHubName: topic}
}

// eventHubReader wraps an Event Hubs partition consumer to implement Reader.
type eventHubReader struct {
	cfg           Config
	eventHubName  string
	consumerGroup string

	consumer  *azeventhubs.ConsumerClient
	partition *azeventhubs.PartitionClient
}

// init lazily creates the consumer and partition client, reusing them across reads.
func (r *eventHubReader) init(ctx context.Context) error {
	if r.partition != nil {
		return nil
	}
	c, err := azeventhubs.NewConsumerClientFromConnectionString(
		r.cfg.connectionString(r.eventHubName), r.eventHubName, r.consumerGroup, nil,
	)
	if err != nil {
		return fmt.Errorf("eventhub: new consumer: %w", err)
	}
	p, err := c.NewPartitionClient(r.cfg.Partition, nil)
	if err != nil {
		c.Close(ctx)
		return fmt.Errorf("eventhub: partition %q: %w", r.cfg.Partition, err)
	}
	r.consumer, r.partition = c, p
	return nil
}

func (r *eventHubReader) ReadMessage(ctx context.Context) (Message, error) {
	msgs, err := r.ReadBatch(ctx, 1)
	if err != nil {
		return Message{}, err
	}
	return msgs[0], nil
}

func (r *eventHubReader) ReadBatch(ctx context.Context, batchSize int) ([]Message, error) {
	if err := r.init(ctx); err != nil {
		return nil, err
	}
	events, err := r.partition.ReceiveEvents(ctx, batchSize, nil)
	if err != nil {
		return nil, fmt.Errorf("eventhub: receive: %w", err)
	}
	out := make([]Message, len(events))
	for i, e := range events {
		out[i] = Message{Topic: r.eventHubName, Value: e.Body}
	}
	return out, nil
}

// Lag is not supported by this Reader (no offset/high-water-mark tracking), so it reports
// 0 - a broadcast consumer treats that as "caught up" and becomes ready immediately.
func (r *eventHubReader) Lag(context.Context) (int64, error) { return 0, nil }

// CommitMessages is a no-op: this Reader does not use an Event Hubs checkpoint
// store, so offsets are not persisted. Wire a checkpoint store here if at-least-once
// delivery across restarts is required.
func (r *eventHubReader) CommitMessages(_ context.Context, _ ...Message) error { return nil }

func (r *eventHubReader) Close() error {
	ctx := context.Background()
	if r.partition != nil {
		r.partition.Close(ctx)
	}
	if r.consumer != nil {
		return r.consumer.Close(ctx)
	}
	return nil
}

// eventHubWriter wraps an Event Hubs producer to implement Writer.
type eventHubWriter struct {
	cfg          Config
	eventHubName string

	producer *azeventhubs.ProducerClient
}

func (w *eventHubWriter) init() error {
	if w.producer != nil {
		return nil
	}
	p, err := azeventhubs.NewProducerClientFromConnectionString(
		w.cfg.connectionString(w.eventHubName), w.eventHubName, nil,
	)
	if err != nil {
		return fmt.Errorf("eventhub: new producer: %w", err)
	}
	w.producer = p
	return nil
}

// WriteMessages packs the message values into Event Hubs batches, flushing a full
// batch and starting a new one when an event won't fit.
func (w *eventHubWriter) WriteMessages(ctx context.Context, msgs ...Message) error {
	if err := w.init(); err != nil {
		return err
	}
	batch, err := w.producer.NewEventDataBatch(ctx, nil)
	if err != nil {
		return fmt.Errorf("eventhub: new batch: %w", err)
	}
	for i := 0; i < len(msgs); i++ {
		err := batch.AddEventData(&azeventhubs.EventData{Body: msgs[i].Value}, nil)
		switch err {
		case nil:
			continue
		case azeventhubs.ErrEventDataTooLarge:
			if batch.NumEvents() == 0 {
				return fmt.Errorf("eventhub: event %d exceeds max batch size", i)
			}
			if err := w.producer.SendEventDataBatch(ctx, batch, nil); err != nil {
				return fmt.Errorf("eventhub: send batch: %w", err)
			}
			if batch, err = w.producer.NewEventDataBatch(ctx, nil); err != nil {
				return fmt.Errorf("eventhub: new batch: %w", err)
			}
			i-- // retry this event in the fresh batch
		default:
			return fmt.Errorf("eventhub: add event: %w", err)
		}
	}
	if batch.NumEvents() > 0 {
		if err := w.producer.SendEventDataBatch(ctx, batch, nil); err != nil {
			return fmt.Errorf("eventhub: send batch: %w", err)
		}
	}
	return nil
}

func (w *eventHubWriter) Close() error {
	if w.producer != nil {
		return w.producer.Close(context.Background())
	}
	return nil
}

// Compile-time interface checks.
var (
	_ Broker = (*eventHubBroker)(nil)
	_ Reader = (*eventHubReader)(nil)
	_ Writer = (*eventHubWriter)(nil)
)

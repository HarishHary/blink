package brokers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	azeventhubs "github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs/v2"
)

// eventHubConfig holds Event Hubs connection credentials and the reader partition.
type eventHubConfig struct {
	Hostname  string `env:"SVC_EVENTHUB_HOST"`
	Username  string `env:"SVC_EVENTHUB_USERNAME"`
	Password  string `env:"SVC_EVENTHUB_PASSWORD"`
	Partition string // partition ID a Reader consumes from
}

func (c eventHubConfig) connectionString(eventHubName string) string {
	return fmt.Sprintf(
		"Endpoint=sb://%s/;SharedAccessKeyName=%s;SharedAccessKey=%s;EntityPath=%s",
		c.Hostname, c.Username, c.Password, eventHubName,
	)
}

// eventHubBroker is the Event Hubs Broker implementation.
type eventHubBroker struct {
	config eventHubConfig
}

// NewEventHubBroker constructs a Broker from connection-level Config.
func NewEventHubBroker(config eventHubConfig) Broker {
	return &eventHubBroker{config: config}
}

// NewReader returns a Reader bound to the given Event Hub (topic) and consumer group.
func (b *eventHubBroker) NewReader(topic, groupID string) Reader {
	return &eventHubReader{config: b.config, eventHubName: topic, consumerGroup: groupID}
}

// NewBroadcastReader returns a reader using Event Hubs' default consumer group.
func (b *eventHubBroker) NewBroadcastReader(topic string) Reader {
	return &eventHubReader{config: b.config, eventHubName: topic, consumerGroup: "$Default", broadcast: true}
}

// NewWriter returns a Writer bound to the given Event Hub (topic).
func (b *eventHubBroker) NewWriter(topic string) Writer {
	return &eventHubWriter{config: b.config, eventHubName: topic}
}

// eventHubReader wraps an Event Hubs partition consumer to implement Reader.
type eventHubReader struct {
	config        eventHubConfig
	eventHubName  string
	consumerGroup string
	broadcast     bool

	consumer  eventHubConsumerAdapter
	partition eventHubPartitionAdapter

	lastSequence int64
	hasSequence  bool
}

// eventHubConsumerAdapter exposes partition state without an Event Hubs namespace in tests.
type eventHubConsumerAdapter interface {
	GetPartitionProperties(context.Context, string, *azeventhubs.GetPartitionPropertiesOptions) (azeventhubs.PartitionProperties, error)
	Close(context.Context) error
}

// eventHubPartitionAdapter exposes partition reads without an Event Hubs namespace in tests.
type eventHubPartitionAdapter interface {
	ReceiveEvents(context.Context, int, *azeventhubs.ReceiveEventsOptions) ([]*azeventhubs.ReceivedEventData, error)
	Close(context.Context) error
}

// init lazily creates the consumer and partition client, reusing them across reads.
func (r *eventHubReader) init(ctx context.Context) error {
	if r.consumer != nil && r.partition != nil {
		return nil
	}
	if r.consumer != nil || r.partition != nil {
		return fmt.Errorf("eventhub: reader is partially initialized")
	}
	c, err := azeventhubs.NewConsumerClientFromConnectionString(
		r.config.connectionString(r.eventHubName), r.eventHubName, r.consumerGroup, nil,
	)
	if err != nil {
		return fmt.Errorf("eventhub: new consumer: %w", err)
	}
	if r.broadcast {
		properties, err := c.GetEventHubProperties(ctx, nil)
		if err != nil {
			_ = c.Close(ctx)
			return fmt.Errorf("eventhub: get properties: %w", err)
		}
		if err := validateEventHubBroadcastPartition(properties.PartitionIDs, r.config.Partition); err != nil {
			_ = c.Close(ctx)
			return err
		}
	}
	p, err := c.NewPartitionClient(r.config.Partition, eventHubPartitionOptions(r.broadcast))
	if err != nil {
		_ = c.Close(ctx)
		return fmt.Errorf("eventhub: partition %q: %w", r.config.Partition, err)
	}
	r.consumer, r.partition = c, p
	return nil
}

func (r *eventHubReader) ReadMessage(ctx context.Context) (Message, error) {
	msgs, err := r.ReadBatch(ctx, 1)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, fmt.Errorf("eventhub: receive returned no events")
	}
	return msgs[0], nil
}

func (r *eventHubReader) ReadBatch(ctx context.Context, batchSize int) ([]Message, error) {
	if batchSize <= 0 {
		return nil, fmt.Errorf("eventhub: batch size must be positive: %d", batchSize)
	}
	if err := r.init(ctx); err != nil {
		return nil, err
	}
	partitionID, err := strconv.Atoi(r.config.Partition)
	if err != nil {
		return nil, fmt.Errorf("eventhub: invalid partition %q: %w", r.config.Partition, err)
	}
	events, err := r.partition.ReceiveEvents(ctx, batchSize, nil)
	if err != nil {
		return nil, fmt.Errorf("eventhub: receive: %w", err)
	}
	out := make([]Message, len(events))
	for i, e := range events {
		var key []byte
		if e.PartitionKey != nil {
			key = []byte(*e.PartitionKey)
		}
		out[i] = Message{Topic: r.eventHubName, Partition: partitionID, Offset: e.SequenceNumber, Key: key, Value: e.Body}
		if !r.hasSequence || e.SequenceNumber > r.lastSequence {
			r.lastSequence = e.SequenceNumber
			r.hasSequence = true
		}
	}
	return out, nil
}

// ReadLag returns the distance from this reader's sequence to the partition tail.
func (r *eventHubReader) ReadLag(ctx context.Context) (int64, error) {
	if err := r.init(ctx); err != nil {
		return 0, err
	}
	properties, err := r.consumer.GetPartitionProperties(ctx, r.config.Partition, nil)
	if err != nil {
		return 0, fmt.Errorf("eventhub: get partition properties: %w", err)
	}
	if properties.IsEmpty {
		return 0, nil
	}
	position := properties.LastEnqueuedSequenceNumber
	if r.broadcast {
		position = properties.BeginningSequenceNumber - 1
	}
	if r.hasSequence {
		position = r.lastSequence
	}
	lag := properties.LastEnqueuedSequenceNumber - position
	if lag < 0 {
		return 0, nil
	}
	return lag, nil
}

// CommitMessages is a no-op because this reader has no checkpoint store.
func (r *eventHubReader) CommitMessages(_ context.Context, _ ...Message) error { return nil }

func (r *eventHubReader) Close() error {
	ctx := context.Background()
	var closeErr error
	if r.partition != nil {
		closeErr = r.partition.Close(ctx)
	}
	if r.consumer != nil {
		if err := r.consumer.Close(ctx); closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func eventHubPartitionOptions(broadcast bool) *azeventhubs.PartitionClientOptions {
	if !broadcast {
		return nil
	}
	earliest := true
	return &azeventhubs.PartitionClientOptions{StartPosition: azeventhubs.StartPosition{Earliest: &earliest}}
}

func validateEventHubBroadcastPartition(partitionIDs []string, configured string) error {
	if len(partitionIDs) != 1 || partitionIDs[0] != configured {
		return fmt.Errorf("eventhub: broadcast snapshots require one partition %q, got %v", configured, partitionIDs)
	}
	return nil
}

// eventHubBatchAdapter exposes Event Hubs batching without a namespace in tests.
type eventHubBatchAdapter interface {
	AddEventData(*azeventhubs.EventData, *azeventhubs.AddEventDataOptions) error
	NumEvents() int32
}

// eventHubProducerAdapter exposes Event Hubs writes without a namespace in tests.
type eventHubProducerAdapter interface {
	NewBatch(context.Context, *string) (eventHubBatchAdapter, error)
	SendBatch(context.Context, eventHubBatchAdapter) error
	Close(context.Context) error
}

type azureEventHubProducer struct {
	client *azeventhubs.ProducerClient
}

func (p *azureEventHubProducer) NewBatch(ctx context.Context, partitionKey *string) (eventHubBatchAdapter, error) {
	var options *azeventhubs.EventDataBatchOptions
	if partitionKey != nil {
		options = &azeventhubs.EventDataBatchOptions{PartitionKey: partitionKey}
	}
	return p.client.NewEventDataBatch(ctx, options)
}

func (p *azureEventHubProducer) SendBatch(ctx context.Context, batch eventHubBatchAdapter) error {
	b, ok := batch.(*azeventhubs.EventDataBatch)
	if !ok {
		return fmt.Errorf("eventhub: invalid batch %T", batch)
	}
	return p.client.SendEventDataBatch(ctx, b, nil)
}

func (p *azureEventHubProducer) Close(ctx context.Context) error { return p.client.Close(ctx) }

// eventHubWriter wraps an Event Hubs producer to implement Writer.
type eventHubWriter struct {
	config       eventHubConfig
	eventHubName string

	producer eventHubProducerAdapter
}

func (w *eventHubWriter) init() error {
	if w.producer != nil {
		return nil
	}
	p, err := azeventhubs.NewProducerClientFromConnectionString(
		w.config.connectionString(w.eventHubName), w.eventHubName, nil,
	)
	if err != nil {
		return fmt.Errorf("eventhub: new producer: %w", err)
	}
	w.producer = &azureEventHubProducer{client: p}
	return nil
}

// WriteMessages packs values into Event Hubs batches and flushes full batches.
func (w *eventHubWriter) WriteMessages(ctx context.Context, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if err := w.init(); err != nil {
		return err
	}
	var batch eventHubBatchAdapter
	var batchKey []byte
	for i := 0; i < len(msgs); {
		if batch == nil || !bytes.Equal(batchKey, msgs[i].Key) {
			if err := w.sendBatch(ctx, batch); err != nil {
				return err
			}
			partitionKey, err := eventHubPartitionKey(msgs[i].Key)
			if err != nil {
				return fmt.Errorf("eventhub: message %d: %w", i, err)
			}
			batch, err = w.producer.NewBatch(ctx, partitionKey)
			if err != nil {
				return fmt.Errorf("eventhub: new batch: %w", err)
			}
			batchKey = append(batchKey[:0], msgs[i].Key...)
		}

		err := batch.AddEventData(&azeventhubs.EventData{Body: msgs[i].Value}, nil)
		if err == nil {
			i++
			continue
		}
		if !errors.Is(err, azeventhubs.ErrEventDataTooLarge) {
			return fmt.Errorf("eventhub: add event: %w", err)
		}
		if batch.NumEvents() == 0 {
			return fmt.Errorf("eventhub: event %d exceeds max batch size", i)
		}
		if err := w.sendBatch(ctx, batch); err != nil {
			return err
		}
		batch = nil
	}
	return w.sendBatch(ctx, batch)
}

func (w *eventHubWriter) sendBatch(ctx context.Context, batch eventHubBatchAdapter) error {
	if batch == nil || batch.NumEvents() == 0 {
		return nil
	}
	if err := w.producer.SendBatch(ctx, batch); err != nil {
		return fmt.Errorf("eventhub: send batch: %w", err)
	}
	return nil
}

func eventHubPartitionKey(key []byte) (*string, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if !utf8.Valid(key) {
		return nil, fmt.Errorf("partition key must be valid UTF-8")
	}
	partitionKey := string(key)
	return &partitionKey, nil
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

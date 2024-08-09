package eventhub

import (
	"context"
	"time"

	eventhub "github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs"
	"github.com/harishhary/blink/internal/errors"
)

type Client struct {
	Configuration

	consumerClient *eventhub.ConsumerClient
}

func New(configuration Configuration) *Client {
	return &Client{Configuration: configuration}
}

func (client *Client) initialize() errors.Error {
	if client.consumerClient != nil {
		return nil
	}

	options := &eventhub.ConsumerClientOptions{}
	hub, err := eventhub.NewConsumerClientFromConnectionString(
		client.connectionString(),
		client.Configuration.EventHubName,
		client.Configuration.ConsumerGroup,
		options,
	)
	if err != nil {
		return errors.NewE(err)
	}

	client.consumerClient = hub
	return nil
}

func (client *Client) Receive() ([]byte, errors.Error) {
	if err := client.initialize(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	partitionClient, err := client.consumerClient.NewPartitionClient(client.Partition, nil)
	if err != nil {
		return nil, errors.NewE(err)
	}
	defer partitionClient.Close(ctx)

	event, err := partitionClient.ReceiveEvents(ctx, 1, nil)
	if err != nil {
		return nil, errors.NewE(err)
	}

	return event[0].Body, nil
}

func (client *Client) ReceiveBatch(maxEvents int) ([][]byte, errors.Error) {
	if err := client.initialize(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	partitionClient, err := client.consumerClient.NewPartitionClient(client.Partition, nil)
	if err != nil {
		return nil, errors.NewE(err)
	}
	defer partitionClient.Close(ctx)

	events, err := partitionClient.ReceiveEvents(ctx, maxEvents, nil)
	if err != nil {
		return nil, errors.NewE(err)
	}

	data := make([][]byte, len(events))
	for i, event := range events {
		data[i] = event.Body
	}

	return data, nil
}

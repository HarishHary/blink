package eventhub

import (
	"context"
	"time"

	eventhub "github.com/Azure/azure-sdk-for-go/sdk/messaging/azeventhubs"
	"github.com/harishhary/blink/internal/errors"
)

type Client struct {
	Configuration

	producerClient *eventhub.ProducerClient
}

func New(configuration Configuration) *Client {
	return &Client{Configuration: configuration}
}

func (client *Client) initialize() errors.Error {
	if client.producerClient != nil {
		return nil
	}

	options := &eventhub.ProducerClientOptions{}
	hub, err := eventhub.NewProducerClientFromConnectionString(
		client.connectionString(),
		client.Configuration.EventHubName,
		options,
	)
	if err != nil {
		return errors.NewE(err)
	}

	client.producerClient = hub
	return nil
}

func (client *Client) Send(data []byte) errors.Error {
	if err := client.initialize(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	batch, err := client.producerClient.NewEventDataBatch(ctx, nil)
	if err != nil {
		return errors.NewE(err)
	}

	err = batch.AddEventData(&eventhub.EventData{Body: data}, nil)
	if err != nil {
		return errors.NewE(err)
	}

	err = client.producerClient.SendEventDataBatch(ctx, batch, nil)
	if err != nil {
		return errors.NewE(err)
	}

	return nil
}

func (client *Client) SendBatch(data ...[]byte) errors.Error {
	if err := client.initialize(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	batch, err := client.producerClient.NewEventDataBatch(ctx, nil)
	if err != nil {
		return errors.NewE(err)
	}

	for i, d := range data {
		err = batch.AddEventData(&eventhub.EventData{Body: d}, nil)

		if err == eventhub.ErrEventDataTooLarge {
			if batch.NumEvents() == 0 {
				// This one event is too large for this batch, even on its own. No matter what we do it
				// will not be sendable at its current size.
				return errors.NewE(err)
			}

			// This batch is full - we can send it and create a new one and continue
			// packaging and sending events.
			if err := client.producerClient.SendEventDataBatch(ctx, batch, nil); err != nil {
				return errors.NewE(err)
			}

			// create the next batch we'll use for events, ensuring that we use the same options
			// each time so all the messages go the same target.
			tmpBatch, err := client.producerClient.NewEventDataBatch(ctx, nil)
			if err != nil {
				return errors.NewE(err)
			}

			batch = tmpBatch

			// rewind so we can retry adding this event to a batch
			i--
		} else if err != nil {
			return errors.NewE(err)
		}
	}

	// if we have any events in the last batch, send it
	if batch.NumEvents() > 0 {
		if err := client.producerClient.SendEventDataBatch(context.TODO(), batch, nil); err != nil {
			panic(err)
		}
	}

	return nil
}

func (client *Client) SendWithID(id string, data []byte) errors.Error {
	if err := client.initialize(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	batch, err := client.producerClient.NewEventDataBatch(ctx, nil)
	if err != nil {
		return errors.NewE(err)
	}

	err = batch.AddEventData(&eventhub.EventData{Body: data, MessageID: &id}, nil)
	if err != nil {
		return errors.NewE(err)
	}

	err = client.producerClient.SendEventDataBatch(ctx, batch, nil)
	if err != nil {
		return errors.NewE(err)
	}

	return nil
}

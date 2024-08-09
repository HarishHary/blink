package elastic

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
	"github.com/harishhary/blink/internal/errors"
)

type IClient interface {
	Index(data [][]byte) errors.Error
}

type Client struct {
	Configuration Configuration

	index string

	client  *elasticsearch.Client
	indexer esutil.BulkIndexer
}

func New(configuration Configuration) *Client {
	return &Client{
		Configuration: configuration,
		index:         fmt.Sprintf("finding_index_%s_%s", configuration.Environment, configuration.Tenant),
	}
}

func (client *Client) initialize() errors.Error {
	if client.client == nil {
		newclient, err := elasticsearch.NewClient(elasticsearch.Config{
			APIKey:  client.Configuration.APIKey,
			CloudID: client.Configuration.CloudID,
		})
		if err != nil {
			return errors.NewE(err)
		}

		client.client = newclient
	}

	if client.indexer == nil {
		indexer, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
			Client:        client.client,
			NumWorkers:    4,
			FlushBytes:    int(5e6),
			FlushInterval: time.Second,
		})
		if err != nil {
			return errors.NewE(err)
		}

		client.indexer = indexer
	}
	return nil
}

func (client *Client) Index(data [][]byte) errors.Error {
	if err := client.initialize(); err != nil {
		return err
	}

	for _, data := range data {
		client.indexer.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Index:  client.index,
				Action: "index",
				Body:   bytes.NewReader(data),
			},
		)
	}

	return nil
}

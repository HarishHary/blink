package azure_storage

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/harishhary/blink/internal/errors"
)

type Client struct {
	Configuration

	blobClient *azblob.Client
}

func New(configuration Configuration) *Client {
	return &Client{
		Configuration: configuration,
	}
}

func (client *Client) initialize() errors.Error {
	if client.blobClient == nil {
		container, err := azblob.NewClientFromConnectionString(client.connectionString(), &azblob.ClientOptions{})
		if err != nil {
			return errors.NewE(err)
		}
		client.blobClient = container
	}

	return nil
}

type Entry struct {
	Name         string
	LastModified time.Time
}

func (client *Client) List(path string) ([]Entry, errors.Error) {
	if err := client.initialize(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	path = strings.TrimLeft(fmt.Sprintf("%s/", path), "/")
	pager := client.blobClient.NewListBlobsFlatPager(client.Container, &azblob.ListBlobsFlatOptions{
		Include: azblob.ListBlobsInclude{Metadata: true, Deleted: true},
		Prefix:  &path,
	})

	entries := make([]Entry, 0)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, errors.NewE(err)
		}
		for _, blob := range page.Segment.BlobItems {
			entries = append(entries, Entry{
				Name:         *blob.Name,
				LastModified: *blob.Properties.LastModified,
			})
		}
	}

	return entries, nil
}

func (client *Client) Upload(blobName string, blobData []byte) errors.Error {
	if err := client.initialize(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	path := strings.TrimLeft(fmt.Sprintf("%s/", blobName), "/")
	blobContentReader := bytes.NewReader(blobData)
	// Perform UploadStream
	_, err := client.blobClient.UploadStream(ctx, client.Container, path, blobContentReader,
		&azblob.UploadStreamOptions{
			Metadata: map[string]*string{"hello": to.Ptr("world")},
		})
	if err != nil {
		return errors.NewE(err)
	}

	return nil
}

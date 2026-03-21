package elastic

import "github.com/harishhary/blink/internal/errors"

type MockClient struct {
	Count int
}

func NewMockClient() *MockClient {
	return &MockClient{}
}

func (client *MockClient) Index(data [][]byte) errors.Error {
	client.Count += len(data)
	return nil
}

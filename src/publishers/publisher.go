package publishers

import (
	"context"
	"fmt"
	"log"
)

// PublisherError custom error for Publisher
type PublisherError struct {
	Message string
}

func (e *PublisherError) Error() string {
	return fmt.Sprintf("Publisher failed with error: %s", e.Message)
}

type IPublishers interface {
	Publish(ctx context.Context, record map[string]interface{}) (bool, error) // FIXME return
}

type BasePublisher struct {
	Name string
}

func (p *BasePublisher) Publish(ctx context.Context, record map[string]interface{}) (bool, error) { // FIXME return
	log.Printf("Using base publisher %s with context: %s. record:%s", p.Name, ctx, record)
	return p.PublishLogic(ctx, record)
}

func (p *BasePublisher) PublishLogic(ctx context.Context, record map[string]interface{}) (bool, error) { // FIXME return
	log.Printf("Using base publisher %s with context: %s. record:%s", p.Name, ctx, record)
	return false, nil
}

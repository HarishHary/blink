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

type IPublisher interface {
	Publish(ctx context.Context, record map[string]interface{}) (bool, error) // FIXME return
	Name() string
}

type Publisher struct {
	Name        string
	PublisherID string
	Description string
	Disabled    bool
}

func (p *Publisher) GetName() string {
	return p.Name
}

func (p *Publisher) Publish(ctx context.Context, record map[string]interface{}) (bool, error) { // FIXME return
	log.Printf("Using base publisher %s with context: %s. record:%s", p.GetName(), ctx, record)
	return p.PublishLogic(ctx, record)
}

func (p *Publisher) PublishLogic(ctx context.Context, record map[string]interface{}) (bool, error) { // FIXME return
	log.Printf("Using base publisher %s with context: %s. record:%s", p.GetName(), ctx, record)
	return false, nil
}

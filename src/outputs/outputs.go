package outputs

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/src/events"
)

type IOutputs interface {
	Dispatch(ctx context.Context, event *events.Event) error
	ComposeAlert(event *events.Event) (*events.Event, error)
}

type SimpleOutput struct{}

func (r *SimpleOutput) Dispatch(ctx context.Context, event *events.Event) error {
	event, err := r.ComposeAlert(event, event)
	if err != nil {
		return nil
	}
	fmt.Println("Simple Ouput from:", event.User.UserName)
	return nil
}

func (r *SimpleOutput) ComposeAlert(event *events.Event, publisher_event *events.Event) (*events.Event, error) {
	default_title := "Incident Title: " + event.User.UserName
	default_html := "Incident Title: " + event.User.UserName
	fmt.Println("Simple Publisher from:", default_title, default_html)
	return publisher_event, nil
}

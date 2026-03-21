package services

import (
	"fmt"

	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
)

type TeamsDispatcher struct {
	dispatchers.Dispatcher
	webhookURL string
}

func (t *TeamsDispatcher) Dispatch(alert alerts.Alert) (bool, errors.Error) {
	// Implement the logic to send alert to Microsoft Teams using the webhookURL
	// This is a placeholder for the actual implementation
	return true, nil
}

func (t *TeamsDispatcher) WebhookURL() string {
	return t.webhookURL
}

func (t *TeamsDispatcher) String() string {
	return fmt.Sprintf("TeamsDispatcher{id: %s, name: %s, description: %s, webhookURL: %s}", t.Id(), t.Name(), t.Description(), t.webhookURL)
}

func NewTeamsDispatcher(name string, config map[string]any) (dispatchers.IDispatcher, errors.Error) {
	if name == "" {
		return nil, errors.New("invalid dispatcher options")
	}

	name = fmt.Sprintf("teams:%s", name)
	webhookURL, ok := config["webhook_url"].(string)
	if !ok || webhookURL == "" {
		return nil, errors.New("missing webhook_url in Teams dispatcher config")
	}

	dispatcher, _ := dispatchers.NewDispatcher(
		name,
		config,
		dispatchers.WithDescription("Sending alerts to Microsoft Teams"),
	)

	return &TeamsDispatcher{
		Dispatcher: *dispatcher,
		webhookURL: webhookURL,
	}, nil
}

func init() {
	dispatchers.RegisterDispatcherConstructor("teams", NewTeamsDispatcher)
}

package enrichments

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/src/enrichments"
	"github.com/harishhary/blink/src/events"
)

// UserEnrichment enriches the event with user data
type UserEnrichment struct {
	timing enrichments.EnrichmentTiming
}

func (e *UserEnrichment) Name() string {
	return "User Enrichment"
}

func (e *UserEnrichment) Enrich(ctx context.Context, event *events.Event) error {
	user, err := getUserData(event.User.UserID)
	if err != nil {
		return err
	}
	event.User = user
	return nil
}

func (e *UserEnrichment) Timing() enrichments.EnrichmentTiming {
	return e.timing
}

func getUserData(userID string) (events.User, error) {
	userDB := map[string]events.User{
		"123": {UserID: "123", UserName: "John Doe", Email: "john.doe@example.com"},
		"456": {UserID: "456", UserName: "Jane Smith", Email: "jane.smith@example.com"},
	}

	user, ok := userDB[userID]
	if !ok {
		return events.User{}, fmt.Errorf("user not found")
	}
	return user, nil
}

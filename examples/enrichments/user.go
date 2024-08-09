package enrichments

import (
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/events"
)

// UserEnrichment enriches the event with user data
type userEnrichment struct {
	enrichments.Enrichment
}

func (e *userEnrichment) Enrich(event events.Event) errors.Error {
	if user, ok := event["UserID"].(string); ok {
		EnrichedUser, err := getUserData(user)
		if err != nil {
			return errors.New(err)
		}
		event["User"] = EnrichedUser
	}
	return nil
}

func getUserData(userID string) (map[string]string, error) {
	userDB := map[string]map[string]string{
		"123": {"UserID": "123", "UserName": "John Doe", "Email": "john.doe@example.com"},
		"456": {"UserID": "456", "UserName": "Jane Smith", "Email": "jane.smith@example.com"},
	}

	user, ok := userDB[userID]
	if !ok {
		return map[string]string{}, fmt.Errorf("user not found")
	}
	return user, nil
}

var userEnrichmentInstance, _ = enrichments.NewEnrichment(
	"User enrichment",
	enrichments.WithDescription("Enrich with user data"),
	enrichments.WithEnabled(false),
)
var UserEnrichment = userEnrichment{
	Enrichment: *userEnrichmentInstance,
}

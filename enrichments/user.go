package enrichments

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/enrichments"
)

// UserEnrichment enriches the event with user data
type userEnrichment struct {
	enrichments.Enrichment
}

func (e *userEnrichment) Enrich(ctx context.Context, record shared.Record) error {
	if user, ok := record["UserID"].(string); ok {
		EnrichedUser, err := getUserData(user)
		if err != nil {
			return err
		}
		record["User"] = EnrichedUser
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

var UserEnrichment = userEnrichment{
	Enrichment: enrichments.NewEnrichment(
		"User enrichment",
		enrichments.Description("Enrich with user data"),
		enrichments.Disabled(false),
	),
}

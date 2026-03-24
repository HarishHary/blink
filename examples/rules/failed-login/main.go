package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules/sdk"
)

// failedLogin fires when a login attempt is recorded as failed.
// Rule metadata (severity, log_types, matchers, etc.) lives in rule.yaml.
//
// It overrides AlertTitle, AlertContext, and AlertSeverity to produce
// richer alerts. All other sdk.BaseRule methods use their default (no-op) values.
type failedLogin struct{ sdk.BaseRule }

func (failedLogin) Evaluate(_ context.Context, event events.Event) (bool, errors.Error) {
	action, _ := event["action"].(string)
	status, _ := event["status"].(string)
	return strings.EqualFold(action, "login") && strings.EqualFold(status, "failed"), nil
}

// AlertTitle produces "Failed login: <username>" using the event's user field.
func (failedLogin) AlertTitle(event events.Event) string {
	user, _ := event["user"].(string)
	if user == "" {
		return "Failed login attempt"
	}
	return fmt.Sprintf("Failed login: %s", user)
}

// AlertContext adds structured fields that enrich the alert for downstream rules and outputs.
func (failedLogin) AlertContext(event events.Event) map[string]any {
	return map[string]any{
		"login_action": event["action"],
		"login_status": event["status"],
		"source_ip":    event["source_ip"],
	}
}

// AlertSeverity escalates to "high" when a failure count field is present and large.
func (failedLogin) AlertSeverity(event events.Event) string {
	if count, ok := event["failure_count"].(float64); ok && count >= 10 {
		return "high"
	}
	return "" // "" → use YAML default ("medium")
}

func main() {
	sdk.Serve(failedLogin{})
}

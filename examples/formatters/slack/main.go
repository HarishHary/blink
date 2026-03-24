package main

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	sdk "github.com/harishhary/blink/pkg/formatters/sdk"
)

// slackFormatter converts an alert dict into a Slack Block Kit payload.
// The host serialises the returned map to JSON and forwards it to the
// configured Slack output.
//
// All static metadata (name, id, enabled, etc.) is declared in
// the companion slack.yaml sidecar file.
type slackFormatter struct{ sdk.BaseFormatter }

// Format receives the full alerts.Alert struct serialised to JSON.
// alerts.Alert has no JSON struct tags, so all field names are PascalCase.
// Event fields (source_ip etc.) are nested under "Event".
func (slackFormatter) Format(_ context.Context, alert map[string]any) (map[string]any, errors.Error) {
	alertID, _ := alert["AlertID"].(string)
	created, _ := alert["Created"].(string)

	// Rule fields are available via alert["Rule"] (a *config.RuleMetadata).
	// Cast it if you need structured access; common fields are already in the event.
	event, _ := alert["Event"].(map[string]any)
	sourceName, _ := event["source_name"].(string)

	header := ":rotating_light: *Alert fired*"
	body := fmt.Sprintf("*Source:* %s\n*Alert ID:* `%s`  •  *Time:* %s", sourceName, alertID, created)

	return map[string]any{
		"text": fmt.Sprintf("Alert fired — %s", alertID),
		"blocks": []map[string]any{
			{
				"type": "header",
				"text": map[string]any{"type": "plain_text", "text": header, "emoji": true},
			},
			{
				"type": "section",
				"text": map[string]any{"type": "mrkdwn", "text": body},
			},
		},
	}, nil
}

func main() {
	sdk.Serve(slackFormatter{})
}

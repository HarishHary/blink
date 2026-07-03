package main

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/formatters"
)

// slackFormatter converts an alert dict into a Slack Block Kit payload.
// The host serialises the returned map to JSON and forwards it to the
// configured Slack output.
type slackFormatter struct{ formatters.BaseFormatter }

func (slackFormatter) Format(_ context.Context, alert map[string]any) (map[string]any, errors.Error) {
	alertID, _ := alert["AlertID"].(string)
	created, _ := alert["Created"].(string)

	event, _ := alert["Event"].(map[string]any)
	sourceName, _ := event["source_name"].(string)

	header := ":rotating_light: *Alert fired*"
	body := fmt.Sprintf("*Source:* %s\n*Alert ID:* `%s`  •  *Time:* %s", sourceName, alertID, created)

	return map[string]any{
		"text": fmt.Sprintf("Alert fired - %s", alertID),
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
	formatters.Serve(slackFormatter{})
}

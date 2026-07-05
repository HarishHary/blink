package main

import (
	"context"
	"fmt"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters"
)

// slackFormatter converts an alert dict into a Slack Block Kit payload.
// The host serialises the returned map to JSON and forwards it to the
// configured Slack output.
type slackFormatter struct{ formatters.BaseFormatter }

func (slackFormatter) Format(_ context.Context, alert alerts.Alert) (map[string]any, errors.Error) {
	sourceName, _ := alert.Event["source_name"].(string)

	header := ":rotating_light: *Alert fired*"
	body := fmt.Sprintf("*Source:* %s\n*Alert ID:* `%s`  •  *Time:* %s", sourceName, alert.Id, alert.Created.Format(time.RFC3339))

	return map[string]any{
		"text": fmt.Sprintf("Alert fired - %s", alert.Id),
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

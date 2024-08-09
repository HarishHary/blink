package osquery_matchers

import (
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

// OsqueryMatcher contains matchers for Osquery events
type OsqueryAddedMatcher struct {
	matchers.Matcher
}

// Added checks if the record action is "added"
func (m *OsqueryAddedMatcher) Match(event events.Event) bool {
	if action, ok := event["action"].(string); ok {
		return action == "added"
	}
	return false
}

const EventTypeLogin = 7

var Runlevels = map[string]struct{}{
	"":         {},
	"LOGIN":    {},
	"reboot":   {},
	"shutdown": {},
	"runlevel": {},
}

type OsqueryUserLoginMatcher struct {
	matchers.Matcher
}

// UserLogin captures user logins from the osquery last table
// This matcher assumes the use of the default osquery pack shipped with the osquery package
// located at /usr/share/osquery/packs/incident-response.conf on the Linux host.
// Update the pack name (rec["name"]) if it is different.
func (m *OsqueryUserLoginMatcher) MatchLogic(event events.Event) bool {
	if name, ok := event["name"].(string); ok && name == "pack_incident-response_last" {
		if columns, ok := event["columns"].(map[string]any); ok {
			if eventType, ok := columns["type"].(int); ok && eventType == EventTypeLogin {
				if username, ok := columns["username"].(string); ok {
					_, exists := Runlevels[username]
					return !exists
				}
			}
		}
	}
	return false
}

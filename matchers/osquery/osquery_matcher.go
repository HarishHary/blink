package matchers

import "github.com/harishhary/blink/src/matchers"

// OsqueryMatcher contains matchers for Osquery events
type OsqueryMatcher struct {
	matchers.BaseMatcher
}

const EventTypeLogin = 7

var Runlevels = map[string]struct{}{
	"":         {},
	"LOGIN":    {},
	"reboot":   {},
	"shutdown": {},
	"runlevel": {},
}

// Added checks if the record action is "added"
func (m *OsqueryMatcher) Added(record map[string]interface{}) bool {
	if action, ok := record["action"].(string); ok {
		return action == "added"
	}
	return false
}

// UserLogin captures user logins from the osquery last table
// This matcher assumes the use of the default osquery pack shipped with the osquery package
// located at /usr/share/osquery/packs/incident-response.conf on the Linux host.
// Update the pack name (rec["name"]) if it is different.
func (m *OsqueryMatcher) UserLogin(record map[string]interface{}) bool {
	if name, ok := record["name"].(string); ok && name == "pack_incident-response_last" {
		if columns, ok := record["columns"].(map[string]interface{}); ok {
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

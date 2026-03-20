// pkg/rules/helpers.go
package rules

import "github.com/harishhary/blink/pkg/events"

// Checks that every required subkey is present in the event. Takes Metadata since it only needs static config: Enabled, ReqSubkeys.
func DefaultSubKeysInEvent(r Metadata, event events.Event) bool {
	if !r.Enabled() {
		return false
	}
	for _, k := range r.ReqSubkeys() {
		if event.Get(k, nil) == nil {
			return false
		}
	}
	return true
}

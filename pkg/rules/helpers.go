// pkg/rules/helpers.go
package rules

import (
	"github.com/harishhary/blink/pkg/events"
)

// DefaultSubKeysInEvent checks that every required subkey is present in the event.
func DefaultSubKeysInEvent(r *RuleMetadata, event events.Event) bool {
	if !r.Enabled {
		return false
	}
	for _, k := range r.ReqSubkeys() {
		if event.Get(k, nil) == nil {
			return false
		}
	}
	return true
}

// pkg/rules/helpers.go
package rules

import (
	"github.com/harishhary/blink/pkg/events"
)

// DefaultSubKeysInEvent checks that every required subkey is present in the event.
func DefaultSubKeysInEvent(rule *RuleMetadata, event events.Event) bool {
	if !rule.Enabled {
		return false
	}
	for _, k := range rule.ReqSubkeys() {
		if event.Get(k, nil) == nil {
			return false
		}
	}
	return true
}

// RulesForLogTypeIn returns the enabled rules in the slice that apply to logType. An empty
// log_types list means the rule applies to all log types.
func RulesForLogTypeIn(rules []*RuleMetadata, logType string) []*RuleMetadata {
	var result []*RuleMetadata
	for _, cfg := range rules {
		if !cfg.Enabled {
			continue
		}
		if len(cfg.LogTypesField) == 0 {
			result = append(result, cfg)
			continue
		}
		for _, lt := range cfg.LogTypesField {
			if lt == logType {
				result = append(result, cfg)
				break
			}
		}
	}
	return result
}

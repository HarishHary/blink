// pkg/rules/helpers.go
package rules

import (
	"slices"

	"github.com/harishhary/blink/pkg/events"
)

// DefaultSubKeysInEvent checks that every required subkey is present in the event.
func DefaultSubKeysInEvent(rule *RuleMetadata, event events.Event) bool {
	if !rule.Enabled {
		return false
	}
	for _, k := range rule.ReqSubkeys {
		if event.Get(k, nil) == nil {
			return false
		}
	}
	return true
}

// RulesForLogTypeIn returns enabled rules that apply to the given log type.
func RulesForLogTypeIn(rules []*RuleMetadata, logType string) []*RuleMetadata {
	var result []*RuleMetadata
	for _, cfg := range rules {
		if !cfg.Enabled {
			continue
		}
		if len(cfg.LogTypes) == 0 {
			result = append(result, cfg)
			continue
		}
		if slices.Contains(cfg.LogTypes, logType) {
			result = append(result, cfg)
		}
	}
	return result
}

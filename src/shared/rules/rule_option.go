package rules

import (
	"github.com/harishhary/blink/src/shared/dispatchers"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/formatters"
	"github.com/harishhary/blink/src/shared/inputs"
	"github.com/harishhary/blink/src/shared/matchers"
	"github.com/harishhary/blink/src/shared/rules/tuning_rules"
)

type RuleOption func(*Rule)

func Description(description string) RuleOption {
	return func(rule *Rule) {
		rule.description = description
	}
}

func Severity(severity int) RuleOption {
	return func(rule *Rule) {
		rule.severity = severity
	}
}

func MergeByKeys(mergeByKeys []string) RuleOption {
	return func(rule *Rule) {
		rule.mergeByKeys = mergeByKeys
	}
}

func MergeWindowMins(mergeWindowMins int) RuleOption {
	return func(rule *Rule) {
		rule.mergeWindowMins = mergeWindowMins
	}
}

func ReqSubkeys(reqSubkeys []string) RuleOption {
	return func(rule *Rule) {
		rule.reqSubkeys = reqSubkeys
	}
}

func Disabled(disabled bool) RuleOption {
	return func(rule *Rule) {
		rule.disabled = disabled
	}
}

func Inputs(inputs []inputs.IInput) RuleOption {
	return func(rule *Rule) {
		rule.inputs = inputs
	}
}

func Dispatchers(dispatchers []dispatchers.IDispatcher) RuleOption {
	return func(rule *Rule) {
		rule.dispatchers = dispatchers
	}
}

func Matchers(matchers []matchers.IMatcher) RuleOption {
	return func(rule *Rule) {
		rule.matchers = matchers
	}
}

func Formatters(formatters []formatters.IFormatter) RuleOption {
	return func(rule *Rule) {
		rule.formatters = formatters
	}
}

func Enrichments(enrichments []enrichments.IEnrichment) RuleOption {
	return func(rule *Rule) {
		rule.enrichments = enrichments
	}
}

func TuningRules(tuningRules []tuning_rules.ITuningRule) RuleOption {
	return func(rule *Rule) {
		rule.tuningRules = tuningRules
	}
}

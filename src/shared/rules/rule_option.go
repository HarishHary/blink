package rules

import (
	"github.com/harishhary/blink/src/shared/dispatchers"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/formatters"
	"github.com/harishhary/blink/src/shared/inputs"
	"github.com/harishhary/blink/src/shared/matchers"
	"github.com/harishhary/blink/src/shared/rules/tuning_rules"
)

type RuleOptions func(*Rule)

func Name(name string) RuleOptions {
	return func(rule *Rule) {
		rule.name = name
	}
}

func Description(description string) RuleOptions {
	return func(rule *Rule) {
		rule.description = description
	}
}

func Severity(severity int) RuleOptions {
	return func(rule *Rule) {
		rule.severity = severity
	}
}

func MergeByKeys(mergeByKeys []string) RuleOptions {
	return func(rule *Rule) {
		rule.mergeByKeys = mergeByKeys
	}
}

func MergeWindowMins(mergeWindowMins int) RuleOptions {
	return func(rule *Rule) {
		rule.mergeWindowMins = mergeWindowMins
	}
}

func ReqSubkeys(reqSubkeys []string) RuleOptions {
	return func(rule *Rule) {
		rule.reqSubkeys = reqSubkeys
	}
}

func Disabled(disabled bool) RuleOptions {
	return func(rule *Rule) {
		rule.disabled = disabled
	}
}

func Inputs(inputs []inputs.IInput) RuleOptions {
	return func(rule *Rule) {
		rule.inputs = inputs
	}
}

func Dispatchers(dispatchers []dispatchers.IDispatcher) RuleOptions {
	return func(rule *Rule) {
		rule.dispatchers = dispatchers
	}
}

func Matchers(matchers []matchers.IMatcher) RuleOptions {
	return func(rule *Rule) {
		rule.matchers = matchers
	}
}

func Formatters(formatters []formatters.IFormatter) RuleOptions {
	return func(rule *Rule) {
		rule.formatters = formatters
	}
}

func Enrichments(enrichments []enrichments.IEnrichment) RuleOptions {
	return func(rule *Rule) {
		rule.enrichments = enrichments
	}
}

func TuningRules(tuningRules []tuning_rules.ITuningRule) RuleOptions {
	return func(rule *Rule) {
		rule.tuningRules = tuningRules
	}
}

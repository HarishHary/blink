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

func Description(Description string) RuleOption {
	return func(rule *Rule) {
		rule.Description = Description
	}
}

func Severity(Severity int) RuleOption {
	return func(rule *Rule) {
		rule.Severity = Severity
	}
}

func MergeByKeys(MergeByKeys []string) RuleOption {
	return func(rule *Rule) {
		rule.MergeByKeys = MergeByKeys
	}
}

func MergeWindowMins(MergeWindowMins int) RuleOption {
	return func(rule *Rule) {
		rule.MergeWindowMins = MergeWindowMins
	}
}

func ReqSubkeys(ReqSubkeys []string) RuleOption {
	return func(rule *Rule) {
		rule.ReqSubkeys = ReqSubkeys
	}
}

func Disabled(Disabled bool) RuleOption {
	return func(rule *Rule) {
		rule.Disabled = Disabled
	}
}

func Inputs(Inputs []inputs.IInput) RuleOption {
	return func(rule *Rule) {
		rule.Inputs = Inputs
	}
}

func Dispatchers(Dispatchers []dispatchers.IDispatcher) RuleOption {
	return func(rule *Rule) {
		rule.Dispatchers = Dispatchers
	}
}

func Matchers(Matchers []matchers.IMatcher) RuleOption {
	return func(rule *Rule) {
		rule.Matchers = Matchers
	}
}

func Formatters(Formatters []formatters.IFormatter) RuleOption {
	return func(rule *Rule) {
		rule.Formatters = Formatters
	}
}

func Enrichments(Enrichments []enrichments.IEnrichment) RuleOption {
	return func(rule *Rule) {
		rule.Enrichments = Enrichments
	}
}

func TuningRules(TuningRules []tuning_rules.ITuningRule) RuleOption {
	return func(rule *Rule) {
		rule.TuningRules = TuningRules
	}
}

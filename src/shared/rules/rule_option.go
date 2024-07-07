package rules

import (
	"github.com/harishhary/blink/src/shared/dispatchers"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/inputs"
	"github.com/harishhary/blink/src/shared/matchers"
	"github.com/harishhary/blink/src/shared/publishers"
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

func DynamicDispatchers(DynamicDispatchers []dispatchers.IDynamicDispatcher) RuleOption {
	return func(rule *Rule) {
		rule.DynamicDispatchers = DynamicDispatchers
	}
}
func Matchers(Matchers []matchers.IMatcher) RuleOption {
	return func(rule *Rule) {
		rule.Matchers = Matchers
	}
}

func Publishers(Publishers []publishers.IPublisher) RuleOption {
	return func(rule *Rule) {
		rule.Publishers = Publishers
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

func InitialContext(InitialContext *map[string]interface{}) RuleOption {
	return func(rule *Rule) {
		rule.InitialContext = InitialContext
	}
}

func Context(Context *map[string]interface{}) RuleOption {
	return func(rule *Rule) {
		rule.Context = Context
	}
}

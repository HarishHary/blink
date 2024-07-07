package tuning_rules

import "github.com/harishhary/blink/src/shared/matchers"

type TuningRuleOption func(*TuningRule)

func Description(Description string) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.Description = Description
	}
}

func Precedence(Precedence int) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.Precedence = Precedence
	}
}

func Disabled(Disabled bool) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.Disabled = Disabled
	}
}

func Matchers(Matchers []matchers.IMatcher) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.Matchers = Matchers
	}
}

func Global(Global bool) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.Global = Global
	}
}

func RuleID(RuleID string) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.RuleID = RuleID
	}
}

func InitialContext(InitialContext *map[string]interface{}) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.InitialContext = InitialContext
	}
}

func Context(Context *map[string]interface{}) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.Context = Context
	}
}

package tuning_rules

import "github.com/harishhary/blink/src/shared/matchers"

type TuningRuleOptions func(*TuningRule)

func Description(description string) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.description = description
	}
}

func Precedence(precedence int) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.precedence = precedence
	}
}

func Disabled(disabled bool) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.disabled = disabled
	}
}

func Matchers(matchers []matchers.IMatcher) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.matchers = matchers
	}
}

func Global(global bool) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.global = global
	}
}

func ID(id string) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.id = id
	}
}

package tuning_rules

import "github.com/harishhary/blink/src/shared/matchers"

type TuningRuleOption func(*TuningRule)

func Description(description string) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.description = description
	}
}

func Precedence(precedence int) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.precedence = precedence
	}
}

func Disabled(disabled bool) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.disabled = disabled
	}
}

func Matchers(matchers []matchers.IMatcher) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.matchers = matchers
	}
}

func Global(global bool) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.global = global
	}
}

func ID(id string) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.id = id
	}
}

func Name(name string) TuningRuleOption {
	return func(rule *TuningRule) {
		rule.name = name
	}
}

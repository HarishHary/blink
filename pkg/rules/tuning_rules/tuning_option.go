package tuning_rules

import "github.com/google/uuid"

type TuningRuleOptions func(*TuningRule)

func WithID(id string) TuningRuleOptions {
	return func(rule *TuningRule) {
		if id == "" {
			rule.id = uuid.NewString()
		} else {
			rule.id = id
		}
	}
}

func WithDescription(description string) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.description = description
	}
}

func WithEnabled(enabled bool) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.enabled = enabled
	}
}

func WithGlobal(global bool) TuningRuleOptions {
	return func(rule *TuningRule) {
		rule.global = global
	}
}

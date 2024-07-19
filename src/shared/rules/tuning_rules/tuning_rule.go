package tuning_rules

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/matchers"
)

// DispatcherError custom error for Dispather
type TuningRuleError struct {
	Message string
}

func (e *TuningRuleError) Error() string {
	return fmt.Sprintf("Tuning rule failed with error: %s", e.Message)
}

type ITuningRule interface {
	Tune(ctx context.Context, record shared.Record) error
	Name() string
}

type TuningRule struct {
	name        string
	id          string
	description string
	precedence  int
	disabled    bool
	matchers    []matchers.IMatcher
	global      bool
}

func (r *TuningRule) Name() string {
	return r.name
}

func NewTuningRule(name string, optFns ...TuningRuleOptions) (*TuningRule, error) {
	if name == "" {
		return nil, &TuningRuleError{Message: "Invalid Tuning Rule options"}
	}
	tuning_rule := &TuningRule{
		name: name,
	}
	for _, optFn := range optFns {
		optFn(tuning_rule)
	}
	return tuning_rule, nil
}

func (r *TuningRule) ApplyMatchers(ctx context.Context, record shared.Record) bool {
	if r.disabled {
		return false
	}

	for _, matcher := range r.matchers {
		match, err := matcher.Match(ctx, record)
		if err != nil {
			return false
		}
		if !match {
			return false // If any matcher fails, do not apply the rule
		}
	}
	return true
}

func (r *TuningRule) Tune(ctx context.Context, record shared.Record) error {
	return nil
}

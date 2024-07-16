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
}

type TuningRule struct {
	Name           string
	RuleID         string
	Description    string
	Precedence     int
	Disabled       bool
	Matchers       []matchers.IMatcher
	Global         bool
	InitialContext *map[string]interface{}
	Context        *map[string]interface{}
}

func NewTuningRule(name string, setters ...TuningRuleOption) TuningRule {
	// Default Options
	r := TuningRule{
		Name: name,
	}
	for _, setter := range setters {
		setter(&r)
	}
	return r
}

func (r *TuningRule) Tune(ctx context.Context, record shared.Record) error {
	if r.Disabled {
		return nil
	}

	for _, matcher := range r.Matchers {
		match, err := matcher.Match(ctx, record)
		if err != nil {
			return &TuningRuleError{Message: err.Error()}
		}
		if !match {
			return nil // If any matcher fails, do not apply the rule
		}
	}

	return r.TuneLogic(ctx, record)
}

func (r *TuningRule) TuneLogic(ctx context.Context, record shared.Record) error {
	return nil
}

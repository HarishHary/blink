package rules

import (
	"context"
)

type ITuningRule interface {
	Tune(ctx context.Context, record map[string]interface{}) bool
}

type BaseTuningRule struct {
	Name        string
	Description string
	Severity    int
	Precedence  int
}

func (r *BaseTuningRule) Tune(ctx context.Context, record map[string]interface{}) bool {
	return r.TuneLogic(ctx, record)
}

func (r *BaseTuningRule) TuneLogic(ctx context.Context, record map[string]interface{}) bool {
	return true
}

package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
)

type RuleType int

const (
	Ignore RuleType = iota
	SetConfidence
	IncreaseConfidence
	DecreaseConfidence
)

func IsValidRuleType(ruleType RuleType) bool {
	switch ruleType {
	case Ignore, SetConfidence, IncreaseConfidence, DecreaseConfidence:
		return true
	default:
		return false
	}
}

type TuningRule interface {
	Tune(ctx context.Context, alerts []alerts.Alert) ([]bool, errors.Error)

	Id() string
	Name() string
	Description() string
	Enabled() bool
	Version() string
	Global() bool
	RuleType() RuleType
	Confidence() scoring.Confidence
	Checksum() string
}

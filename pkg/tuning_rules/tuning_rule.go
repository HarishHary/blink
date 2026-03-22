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
	Tune(ctx context.Context, alrts []alerts.Alert) ([]bool, errors.Error)

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

// ProcessTuningRules applies tuning rules in priority order: Ignore > SetConfidence > Increase/Decrease.
// Returns (confidence, ignored, err). When ignored=true the alert should be discarded.
func ProcessTuningRules(ctx context.Context, alert alerts.Alert, rules []TuningRule) (scoring.Confidence, bool, errors.Error) {
	confidence := alert.Confidence
	batch := []alerts.Alert{alert}

	for _, rule := range rules {
		if rule.RuleType() == Ignore {
			results, err := rule.Tune(ctx, batch)
			if err != nil {
				return confidence, false, err
			}
			if results[0] {
				return 0, true, nil
			}
		}
	}

	setByRule := false
	for _, rule := range rules {
		if rule.RuleType() == SetConfidence {
			results, err := rule.Tune(ctx, batch)
			if err != nil {
				return confidence, false, err
			}
			if results[0] {
				if !setByRule || rule.Confidence() > confidence {
					confidence = rule.Confidence()
					setByRule = true
				}
			}
		}
	}

	if setByRule {
		return confidence, false, nil
	}

	for _, rule := range rules {
		if rule.RuleType() == IncreaseConfidence || rule.RuleType() == DecreaseConfidence {
			results, err := rule.Tune(ctx, batch)
			if err != nil {
				return confidence, false, err
			}
			if results[0] {
				if rule.RuleType() == IncreaseConfidence && rule.Confidence() > confidence {
					confidence = rule.Confidence()
				} else if rule.RuleType() == DecreaseConfidence && rule.Confidence() < confidence {
					confidence = rule.Confidence()
				}
			}
		}
	}

	return confidence, false, nil
}

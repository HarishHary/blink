package tuning_rules

import (
	"github.com/google/uuid"
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

type ITuningRule interface {
	Tune(alert alerts.Alert) (bool, errors.Error)

	// Getters
	Id() string
	Name() string
	Description() string
	Enabled() bool
	Global() bool
	RuleType() RuleType
	Confidence() scoring.Confidence
}

type TuningRule struct {
	id          string
	name        string
	description string
	enabled     bool
	global      bool
	ruleType    RuleType
	confidence  scoring.Confidence
}

func (r *TuningRule) Id() string {
	return r.id
}

func (r *TuningRule) Name() string {
	return r.name
}

func (r *TuningRule) Description() string {
	return r.description
}

func (r *TuningRule) Enabled() bool {
	return r.enabled
}

func (r *TuningRule) Global() bool {
	return r.global
}

func (r *TuningRule) RuleType() RuleType {
	return r.ruleType
}

func (r *TuningRule) Confidence() scoring.Confidence {
	return r.confidence
}

func NewTuningRule(name string, ruleType RuleType, confidence scoring.Confidence, optFns ...TuningRuleOptions) (*TuningRule, errors.Error) {
	if name == "" {
		return nil, errors.New("invalid tuning rule options: non empty name is required")
	}
	if !IsValidRuleType(ruleType) {
		return nil, errors.New("invalid tuning rule options: invalid rule type")
	}
	if !scoring.IsValidConfidence(confidence) {
		return nil, errors.New("invalid tuning rule options: invalid confidence value")
	}
	tuning_rule := &TuningRule{
		name:        name,
		id:          uuid.NewString(),
		description: "Unknown description",
		enabled:     true,
		global:      false,
		ruleType:    ruleType,
		confidence:  confidence,
	}
	for _, optFn := range optFns {
		optFn(tuning_rule)
	}
	return tuning_rule, nil
}

func (r *TuningRule) Tune(alert alerts.Alert) (bool, errors.Error) {
	if !r.enabled {
		return false, nil
	}
	return true, nil
}

func ProcessTuningRules(alert alerts.Alert, rules []ITuningRule) (scoring.Confidence, errors.Error) {
	confidence := alert.Confidence

	// Process Ignore rules first
	for _, rule := range rules {
		if rule.RuleType() == Ignore {
			applies, err := rule.Tune(alert)
			if err != nil {
				return confidence, err
			}
			if applies {
				return "", nil // If any ignore rule is matched, return empty confidence
			}
		}
	}

	// Process SetConfidence rules next
	for _, rule := range rules {
		if rule.RuleType() == SetConfidence {
			applies, err := rule.Tune(alert)
			if err != nil {
				return confidence, err
			}
			if applies {
				if confidence == "" || rule.Confidence() > confidence {
					confidence = rule.Confidence()
				}
			}
		}
	}

	if confidence != "" {
		return confidence, nil // If a SetConfidence rule has been applied, return that confidence
	}

	// Process Increase/Decrease confidence rules last
	for _, rule := range rules {
		if rule.RuleType() == IncreaseConfidence || rule.RuleType() == DecreaseConfidence {
			applies, err := rule.Tune(alert)
			if err != nil {
				return confidence, err
			}
			if applies {
				// Increase or decrease confidence for the final value
				if confidence == "" {
					confidence = rule.Confidence()
				} else {
					if rule.RuleType() == IncreaseConfidence && rule.Confidence() > confidence {
						confidence = rule.Confidence()
					} else if rule.RuleType() == DecreaseConfidence && rule.Confidence() < confidence {
						confidence = rule.Confidence()
					}
				}
			}
		}
	}

	return confidence, nil
}

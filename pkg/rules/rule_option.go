package rules

import (
	"time"

	"github.com/harishhary/blink/pkg/scoring"
)

type RuleOptions func(*Rule)

func WithDescription(description string) RuleOptions {
	return func(rule *Rule) {
		rule.description = description
	}
}

func WithSeverity(severity scoring.Severity) RuleOptions {
	return func(rule *Rule) {
		if !scoring.IsValidSeverity(severity) {
			severity = scoring.SeverityEnum.Info
		}
		rule.severity = severity
	}
}

func WithConfidence(confidence scoring.Confidence) RuleOptions {
	return func(rule *Rule) {
		if !scoring.IsValidConfidence(confidence) {
			confidence = scoring.ConfidenceEnum.VeryLow
		}
		rule.confidence = confidence
	}
}

func WithSignalThreshold(signalThreshold scoring.Confidence) RuleOptions {
	return func(rule *Rule) {
		if !scoring.IsValidConfidence(signalThreshold) {
			signalThreshold = scoring.ConfidenceEnum.VeryLow
		}
		rule.signal = false
		rule.signalThreshold = signalThreshold
	}
}

func WithTags(tags []string) RuleOptions {
	return func(rule *Rule) {
		rule.tags = tags
	}
}

func WithMergeByKeys(mergeByKeys []string) RuleOptions {
	return func(rule *Rule) {
		rule.mergeByKeys = mergeByKeys
	}
}

func WithMergeWindowMins(mergeWindowMins time.Duration) RuleOptions {
	return func(rule *Rule) {
		rule.mergeWindowMins = mergeWindowMins
	}
}

func WithReqSubkeys(reqSubkeys []string) RuleOptions {
	return func(rule *Rule) {
		rule.reqSubkeys = reqSubkeys
	}
}

func WithEnabled(enabled bool) RuleOptions {
	return func(rule *Rule) {
		rule.enabled = enabled
	}
}

func WithDispatchers(dispatchers []string) RuleOptions {
	return func(rule *Rule) {
		rule.dispatchers = dispatchers
	}
}

func WithMatchers(matchers []string) RuleOptions {
	return func(rule *Rule) {
		rule.matchers = matchers
	}
}

func WithFormatters(formatters []string) RuleOptions {
	return func(rule *Rule) {
		rule.formatters = formatters
	}
}

func WithEnrichments(enrichments []string) RuleOptions {
	return func(rule *Rule) {
		rule.enrichments = enrichments
	}
}

func WithTuningRules(tuningRules []string) RuleOptions {
	return func(rule *Rule) {
		rule.tuningRules = tuningRules
	}
}

package main

import (
	"context"
	"fmt"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/rules/tuning_rules"
	"github.com/harishhary/blink/pkg/scoring"
)

// SampleRule is an example implementation of the DetectionRule interface.
type sampleRule struct {
	rules.Rule
}

func (r *sampleRule) Evaluate(ctx context.Context, record events.Event) bool {
	// Implement your rule evaluation logic here
	fmt.Println("Evaluating SampleRule for event:", ctx)
	return true
}

type sampleTuningRule struct {
	tuning_rules.TuningRule
}

func (r *sampleTuningRule) Tune(ctx context.Context, alert alerts.Alert) (bool, errors.Error) {
	// Implement your rule evaluation logic here
	fmt.Println("Tuning sampleTuningRule for event:", ctx)
	return false, nil
}

var Tuning_rule, _ = tuning_rules.NewTuningRule(
	"Sample tuning rule",
	tuning_rules.SetConfidence,
	scoring.ConfidenceEnum.Medium,
)
var SampletuningRule = sampleTuningRule{
	TuningRule: *Tuning_rule,
}

var rule, _ = rules.NewRule("SampleRule",
	rules.WithDescription("This is my custom rule."),
	rules.WithSeverity(scoring.SeverityEnum.Low),
	rules.WithEnrichments([]string{
		"Geo Location enrichment",
		"User enrichment",
	}),
	rules.WithTuningRules([]string{
		"Sample tuning rule",
	}),
	rules.WithFormatters([]string{
		"Uppercase formatter 1",
	}),
)

var Plugin = sampleRule{
	Rule: *rule,
}

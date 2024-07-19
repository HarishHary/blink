package main

import (
	"context"
	"fmt"

	global_enrichment "github.com/harishhary/blink/enrichments"
	global_formatters "github.com/harishhary/blink/formatters"
	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/formatters"
	"github.com/harishhary/blink/src/shared/rules"
	"github.com/harishhary/blink/src/shared/rules/tuning_rules"
)

// SampleRule is an example implementation of the DetectionRule interface.
type sampleRule struct {
	rules.Rule
}

func (r *sampleRule) Evaluate(ctx context.Context, record shared.Record) bool {
	// Implement your rule evaluation logic here
	fmt.Println("Evaluating SampleRule for event:", ctx)
	return true
}

type sampleTuningRule struct {
	tuning_rules.TuningRule
}

func (r *sampleTuningRule) Tune(ctx context.Context, record shared.Record) error {
	// Implement your rule evaluation logic here
	fmt.Println("Tuning sampleTuningRule for event:", ctx)
	return nil
}

var tuning_rule, _ = tuning_rules.NewTuningRule(
	"Sample tuning rule",
)
var sampletuningRule = sampleTuningRule{
	TuningRule: *tuning_rule,
}

var rule, _ = rules.NewRule("SampleRule",
	rules.Description("This is my custom rule."),
	rules.Severity(5),
	rules.Enrichments([]enrichments.IEnrichment{
		&global_enrichment.GeoLocationEnrichment,
		&global_enrichment.UserEnrichment,
	}),
	rules.TuningRules([]tuning_rules.ITuningRule{
		&sampletuningRule,
	}),
	rules.Formatters([]formatters.IFormatter{
		&global_formatters.UppercaseFormatter,
	}),
)
var Rule = &sampleRule{
	Rule: *rule,
}

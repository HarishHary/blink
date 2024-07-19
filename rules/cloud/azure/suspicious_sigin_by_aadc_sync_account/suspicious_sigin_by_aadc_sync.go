package main

import (
	"context"
	"fmt"

	global_enrichment "github.com/harishhary/blink/enrichments"
	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/rules"
	"github.com/harishhary/blink/src/shared/rules/tuning_rules"
)

type SuspiciousSignInByAadcSync struct {
	rules.Rule
}

func (r *SuspiciousSignInByAadcSync) Evaluate(ctx context.Context, record shared.Record) bool {
	// Implement your rule evaluation logic here
	fmt.Println("Evaluating SampleRule for event:", ctx)
	return true
}

var rule, _ = rules.NewRule("Suspicious Sigin",
	rules.Description("This is my custom rule 2."),
	rules.Severity(1),
	rules.Enrichments([]enrichments.IEnrichment{
		&global_enrichment.GeoLocationEnrichment,
		&global_enrichment.UserEnrichment,
	}),
	rules.TuningRules([]tuning_rules.ITuningRule{}),
)

func New() rules.IRule {
	return &SuspiciousSignInByAadcSync{
		Rule: *rule,
	}
}

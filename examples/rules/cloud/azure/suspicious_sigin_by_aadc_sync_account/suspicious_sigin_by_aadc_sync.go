package main

import (
	"fmt"

	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/scoring"
)

type SuspiciousSignInByAadcSync struct {
	rules.Rule
}

func (r *SuspiciousSignInByAadcSync) Evaluate(event events.Event) bool {
	// Implement your rule evaluation logic here
	fmt.Printf("Evaluating SampleRule for event: %v", event)
	return true
}

var rule, _ = rules.NewRule("Suspicious Sigin",
	rules.WithDescription("This is my custom rule 2."),
	rules.WithSeverity(scoring.SeverityEnum.Low),
	rules.WithEnrichments([]string{
		"Geo Location enrichment",
		"User enrichment",
	}),
	rules.WithTuningRules([]string{}),
)

var Plugin = SuspiciousSignInByAadcSync{
	Rule: *rule,
}

package main

import (
	"context"
	"fmt"

	global_enrichment "github.com/harishhary/blink/enrichments"
	"github.com/harishhary/blink/src/enrichments"
	"github.com/harishhary/blink/src/events"
	"github.com/harishhary/blink/src/rules"
)

type SuspiciousSignInByAadcSync struct {
	rules.DetectionRule
}

func (r *SuspiciousSignInByAadcSync) EvaluateLogic(ctx context.Context, event *events.Event) bool {
	// Implement your rule evaluation logic here
	fmt.Println("Evaluating SampleRule for event:", event)
	return true
}

func New() rules.IDetectionRule {
	return &SuspiciousSignInByAadcSync{
		DetectionRule: rules.DetectionRule{
			Name:        "Suspicious Sigin",
			Description: "This is my custom rule 2.",
			Severity:    1,
			Enrichments: []enrichments.IEnrichmentFunction{
				&global_enrichment.GeoLocationEnrichment{},
				&global_enrichment.UserEnrichment{},
			},
			TuningRules: []rules.ITuningRule{},
		},
	}
}

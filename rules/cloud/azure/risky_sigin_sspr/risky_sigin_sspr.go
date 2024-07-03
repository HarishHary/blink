package main

// import (
// 	"context"
// 	"fmt"

	global_enrichment "github.com/harishhary/blink/enrichments"
	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/rules"
	"github.com/harishhary/blink/src/shared/rules/tuning_rules"
)

// SampleRule is an example implementation of the DetectionRule interface.
type SampleRule struct {
	rules.Rule
}

func (r *SampleRule) EvaluateLogic(ctx context.Context, record shared.Record) bool {
	// Implement your rule evaluation logic here
	fmt.Println("Evaluating SampleRule for event:", ctx)
	return true
}

func New() rules.IRule {
	return &SampleRule{
		Rule: rules.Rule{
			Name:        "MyRule",
			Description: "This is my custom rule.",
			Severity:    5,
			Enrichments: []enrichments.IEnrichment{
				&global_enrichment.GeoLocationEnrichment{},
				&global_enrichment.UserEnrichment{},
			},
			TuningRules: []tuning_rules.ITuningRule{},
		},
	}
}

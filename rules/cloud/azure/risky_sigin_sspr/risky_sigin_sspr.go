package main

// import (
// 	"context"
// 	"fmt"

// 	global_enrichment "github.com/harishhary/blink/enrichments"
// 	"github.com/harishhary/blink/src/enrichments"
// 	"github.com/harishhary/blink/src/events"
// 	"github.com/harishhary/blink/src/rules"
// )

// // SampleRule is an example implementation of the DetectionRule interface.
// type SampleRule struct {
// 	rules.DetectionRule
// }

// func (r *SampleRule) EvaluateLogic(ctx context.Context, event *events.Event) bool {
// 	// Implement your rule evaluation logic here
// 	fmt.Println("Evaluating SampleRule for event:", event)
// 	return true
// }

// func New() rules.IDetectionRule {
// 	return &SampleRule{
// 		DetectionRule: rules.DetectionRule{
// 			Name:        "MyRule",
// 			Description: "This is my custom rule.",
// 			Severity:    5,
// 			Enrichments: []enrichments.IEnrichmentFunction{
// 				&global_enrichment.GeoLocationEnrichment{},
// 				&global_enrichment.UserEnrichment{},
// 			},
// 			TuningRules: []rules.ITuningRule{},
// 		},
// 	}
// }

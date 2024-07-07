package main

// import (
// 	"context"
// 	"fmt"
// import (
// 	"context"
// 	"fmt"

	global_enrichment "github.com/harishhary/blink/enrichments"
	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/rules"
	"github.com/harishhary/blink/src/shared/rules/tuning_rules"
)

type SuspiciousSignInByAadcSync struct {
	rules.Rule
}

func (r *SuspiciousSignInByAadcSync) EvaluateLogic(ctx context.Context, record shared.Record) bool {
	// Implement your rule evaluation logic here
	fmt.Println("Evaluating SampleRule for event:", ctx)
	return true
}

func New() rules.IRule {
	return &SuspiciousSignInByAadcSync{
		Rule: rules.Rule{
			Name:        "Suspicious Sigin",
			Description: "This is my custom rule 2.",
			Severity:    1,
			Enrichments: []enrichments.IEnrichment{
				&global_enrichment.GeoLocationEnrichment{},
				&global_enrichment.UserEnrichment{},
			},
			TuningRules: []tuning_rules.ITuningRule{},
		},
	}
}

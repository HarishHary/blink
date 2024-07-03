package rules

import (
	"context"
	"log"

	global_enrichments "github.com/harishhary/blink/enrichments"
	"github.com/harishhary/blink/src/dispatchers"
	"github.com/harishhary/blink/src/enrichments"
	"github.com/harishhary/blink/src/helpers"
)

type ExampleRule struct {
	BaseRule
}

// NewExampleDispatcher creates a new instance of ExampleDispatcher
func NewExampleRule(config map[string]interface{}) *ExampleRule {
	rule := new(ExampleRule)
	enrichFunctions := []enrichments.IEnrichmentFunction{
		&global_enrichments.GeoLocationEnrichment{
			BaseEnrichmentFunction: enrichments.BaseEnrichmentFunction{
				Name:   "test",
				Timing: 1,
			},
		},
	}
	dispatcherFunctions := []dispatchers.IDispatcher{
		&dispatchers.ExampleDispatcher{
			BaseDispatcher: dispatchers.BaseDispatcher{
				ServiceName:   "test",
				ServiceURL:    "test",
				RequestHelper: &helpers.RequestHelper{},
			},
		},
	}

	rule.Enrichments = enrichFunctions
	rule.Dispatchers = dispatcherFunctions
	return rule
}

func (d *ExampleRule) EvaluateLogic(ctx context.Context, record map[string]interface{}) bool {
	log.Printf("Evaluating rule %s with %s with record %s", d.Name, ctx, record)
	return false
}

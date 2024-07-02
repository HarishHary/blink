package rules

import (
	"context"
	"fmt"
	"plugin"

	"github.com/harishhary/blink/src/dispatchers"
	"github.com/harishhary/blink/src/enrichments"
	"github.com/harishhary/blink/src/events"
	"github.com/harishhary/blink/src/inputs"
	"github.com/harishhary/blink/src/matchers"
	"github.com/harishhary/blink/src/publishers"
)

type IDetectionRule interface {
	Evaluate(ctx context.Context, event *events.Event) bool
}

type DetectionRule struct {
	Name        string
	Description string
	Severity    int
	Inputs      []inputs.IInput
	Dispathers  []dispatchers.IDispatcher
	Matchers    []matchers.IMatchers
	Publishers  []publishers.IPublishers
	Enrichments []enrichments.IEnrichmentFunction
	TuningRules []ITuningRule
}

// ApplyEnrichments applies all enrichment functions to the event.
func (r *DetectionRule) ApplyEnrichments(ctx context.Context, event *events.Event) error {
	for _, enrich := range r.Enrichments {
		enrich.Enrich(ctx, event)
	}
	return nil
}

// ApplyTuningRules applies all tuning rules to the event.
func (r *DetectionRule) ApplyTuningRules(ctx context.Context, event *events.Event) error {
	for _, tune := range r.TuningRules {
		tune.Tune(ctx, event)
	}
	return nil
}

// ApplyPublishers applies all publishers to the event.
func (r *DetectionRule) ApplyPublishers(ctx context.Context, event *events.Event) error {
	for _, publisher := range r.Publishers {
		publisher.Publish(ctx, event)
	}
	return nil
}

func LoadPlugins[T any](paths []string) ([]T, error) {
	var plugins []T
	for _, path := range paths {
		p, err := plugin.Open(path)
		if err != nil {
			return nil, err
		}
		sym, err := p.Lookup("Plugin")
		if err != nil {
			return nil, err
		}
		pluginInstance, ok := sym.(T)
		if !ok {
			return nil, fmt.Errorf("invalid type for plugin %s", path)
		}
		plugins = append(plugins, pluginInstance)
	}
	return plugins, nil
}

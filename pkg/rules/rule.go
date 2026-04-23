package rules

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/events"
)

type PluginMetadata = plugin.PluginMetadata

// EvalResult is the per-event outcome returned by Rule.Evaluate.
// Fields beyond Matched are populated only when the plugin implements the
// corresponding optional capability interface (Titler, Describer, etc.).
// An empty/zero field means "use the YAML-configured default".
type EvalResult struct {
	Matched     bool
	Title       string
	Description string
	Severity    string         // "" = no override; "info"/"low"/"medium"/"high"/"critical" = override
	Context     map[string]any // extra key-value pairs merged into alert.Event
	MergeByKeys []string       // overrides YAML merge_by_keys when non-nil
}

// Rule is the full interface for live rule plugins: config accessor + batch evaluation.
// All rules receive a slice of events and return one EvalResult per event.
// PluginMetadata + Checksum together satisfy plugin.Syncable.
type Rule interface {
	RuleMetadata() *RuleMetadata
	Metadata() PluginMetadata
	Checksum() string
	Evaluate(ctx context.Context, evts []events.Event) ([]EvalResult, errors.Error)
}

package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
	"github.com/harishhary/blink/pkg/tuning_rules/config"
)

// PluginMetadata is re-exported from internal/plugin so plugin authors don't need to
// import an internal package.
type PluginMetadata = plugin.PluginMetadata

type RuleType int

const (
	Ignore RuleType = iota
	SetConfidence
	IncreaseConfidence
	DecreaseConfidence
)

type TuningRule interface {
	Tune(ctx context.Context, alerts []alerts.Alert) ([]bool, errors.Error)

	TuningMetadata() *config.TuningMetadata
	PluginMetadata() plugin.PluginMetadata // satisfies plugin.Syncable
	Global() bool
	RuleType() RuleType
	Confidence() scoring.Confidence
	Checksum() string
}

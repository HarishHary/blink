package tuning_rules

import (
	"context"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
)

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

	TuningRuleMetadata() *TuningRuleMetadata
	Metadata() PluginMetadata
	Global() bool
	RuleType() RuleType
	Confidence() scoring.Confidence
	Checksum() string
}

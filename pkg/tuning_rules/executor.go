package tuning_rules

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var tuningExecutorMetrics = plugin.NewPluginExecutorMetrics("tuning_rules")

type TuningRulePluginExecutor = plugin.PluginExecutor[TuningRule]

func NewTuningRulePluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*TuningRuleMetadata]) *TuningRulePluginExecutor {
	return plugin.NewPluginExecutor[TuningRule](logger, notify, dir, src, NewTuningRuleAdapter(cfg), tuningExecutorMetrics)
}

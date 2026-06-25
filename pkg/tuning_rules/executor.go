package tuning_rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var tuningExecutorMetrics = plugin.NewPluginExecutorMetrics("tuning_rules")

type TuningRulePluginExecutor = plugin.PluginExecutor[TuningRule]

func NewTuningRulePluginExecutor(log *logger.Logger, notify plugin.Notify, dir string, snap plugin.SnapshotSource, manager *TuningRuleConfigWatcher) *TuningRulePluginExecutor {
	return plugin.NewPluginExecutor[TuningRule](log, notify, dir, snap, NewTuningRuleAdapter(manager), tuningExecutorMetrics)
}

package tuning_rules

import (
	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/logger"
)

var tuningExecutorMetrics = executor.NewPluginExecutorMetrics("tuning_rules")

type TuningRulePluginExecutor = executor.PluginExecutor[TuningRule]

func NewTuningRulePluginExecutor(log *logger.Logger, notify executor.Notify, dir string, manager *TuningRuleConfigManager) *TuningRulePluginExecutor {
	return executor.NewPluginExecutor[TuningRule](log, notify, dir, NewTuningRuleAdapter(manager), tuningExecutorMetrics)
}

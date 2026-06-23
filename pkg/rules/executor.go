package rules

import (
	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/logger"
)

var ruleExecutorMetrics = executor.NewPluginExecutorMetrics("rulesvc")

type RulePluginExecutor = executor.PluginExecutor[Rule]

func NewRulePluginExecutor(log *logger.Logger, notify executor.Notify, dir string, manager *RuleConfigManager) *RulePluginExecutor {
	return executor.NewPluginExecutor[Rule](log, notify, dir, NewRuleAdapter(manager), ruleExecutorMetrics)
}

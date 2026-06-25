package rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var ruleExecutorMetrics = plugin.NewPluginExecutorMetrics("rulesvc")

type RulePluginExecutor = plugin.PluginExecutor[Rule]

func NewRulePluginExecutor(log *logger.Logger, notify plugin.Notify, dir string, snap plugin.SnapshotSource, manager *RuleConfigWatcher) *RulePluginExecutor {
	return plugin.NewPluginExecutor[Rule](log, notify, dir, snap, NewRuleAdapter(manager), ruleExecutorMetrics)
}

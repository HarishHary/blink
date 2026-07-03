package rules

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var ruleExecutorMetrics = plugin.NewPluginExecutorMetrics("rules")

type RulePluginExecutor = plugin.PluginExecutor[Rule]

func NewRulePluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*RuleMetadata]) *RulePluginExecutor {
	return plugin.NewPluginExecutor[Rule](logger, notify, dir, src, NewRuleAdapter(cfg), ruleExecutorMetrics)
}

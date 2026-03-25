package rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var ruleManagerMetrics = plugin.NewPluginManagerMetrics("rulesvc")

type RulePluginManager = plugin.PluginManager[Rule]

func NewRulePluginManager(log *logger.Logger, notify plugin.Notify, dir string, manager *RuleConfigManager) *RulePluginManager {
	return plugin.NewPluginManager[Rule](log, notify, dir, &RuleAdapter{Manager: manager}, ruleManagerMetrics)
}

package rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/rules/config"
)

var ruleManagerMetrics = plugin.NewPluginManagerMetrics("rulesvc")

func NewManager(log *logger.Logger, notify plugin.Notify, dir string, watcher *config.Watcher) *plugin.PluginManager[Rule] {
	return plugin.NewPluginManager[Rule](log, notify, dir, &RuleAdapter{Watcher: watcher}, ruleManagerMetrics)
}

package rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pluginmgr"
	"github.com/harishhary/blink/pkg/rules/config"
)

var ruleManagerMetrics = pluginmgr.NewManagerMetrics("rulesvc")

func NewManager(log *logger.Logger, notify pluginmgr.Notify, dir string, watcher *config.Watcher) *pluginmgr.PluginManager[Rule] {
	return pluginmgr.NewPluginManager[Rule](log, notify, dir, &RuleAdapter{Watcher: watcher}, ruleManagerMetrics)
}

package tuning_rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pluginmgr"
)

var tuningManagerMetrics = pluginmgr.NewPluginManagerMetrics("tuning_rules")

func NewManager(log *logger.Logger, notify pluginmgr.Notify, dir string) pluginmgr.Plugin {
	return pluginmgr.NewPluginManager[TuningRule](log, notify, dir, &TuningRuleAdapter{}, tuningManagerMetrics)
}

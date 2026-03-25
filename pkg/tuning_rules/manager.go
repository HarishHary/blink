package tuning_rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var tuningManagerMetrics = plugin.NewPluginManagerMetrics("tuning_rules")

type TuningRulePluginManager = plugin.PluginManager[TuningRule]

func NewTuningRulePluginManager(log *logger.Logger, notify plugin.Notify, dir string, manager *TuningRuleConfigManager) *TuningRulePluginManager {
	return plugin.NewPluginManager[TuningRule](log, notify, dir, &TuningRuleAdapter{Manager: manager}, tuningManagerMetrics)
}

package tuning_rules

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/tuning_rules/config"
)

var tuningManagerMetrics = plugin.NewPluginManagerMetrics("tuning_rules")

func NewManager(log *logger.Logger, notify plugin.Notify, dir string, watcher *config.Watcher) *plugin.PluginManager[TuningRule] {
	return plugin.NewPluginManager[TuningRule](log, notify, dir, &TuningRuleAdapter{Watcher: watcher}, tuningManagerMetrics)
}

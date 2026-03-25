package formatters

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var formatterManagerMetrics = plugin.NewPluginManagerMetrics("formatters")

type FormaterPluginManager = plugin.PluginManager[Formatter]

func NewFormatterPluginManager(log *logger.Logger, notify plugin.Notify, dir string, manager *FormatterConfigManager) *FormaterPluginManager {
	return plugin.NewPluginManager[Formatter](log, notify, dir, &FormatterAdapter{Manager: manager}, formatterManagerMetrics)
}

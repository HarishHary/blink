package formatters

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pluginmgr"
)

var formatterManagerMetrics = pluginmgr.NewPluginManagerMetrics("formatters")

func NewManager(log *logger.Logger, notify pluginmgr.Notify, dir string) pluginmgr.Plugin {
	return pluginmgr.NewPluginManager[IFormatter](log, notify, dir, &FormatterAdapter{}, formatterManagerMetrics)
}

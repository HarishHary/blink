package formatters

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/formatters/config"
)

var formatterManagerMetrics = plugin.NewPluginManagerMetrics("formatters")

func NewManager(log *logger.Logger, notify plugin.Notify, dir string, watcher *config.Watcher) *plugin.PluginManager[Formatter] {
	return plugin.NewPluginManager[Formatter](log, notify, dir, &FormatterAdapter{Watcher: watcher}, formatterManagerMetrics)
}

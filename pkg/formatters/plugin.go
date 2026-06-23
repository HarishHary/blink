package formatters

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var formatterExecutorMetrics = plugin.NewPluginManagerMetrics("formatters")

type FormatterPluginExecutor = plugin.PluginExecutor[Formatter]

func NewFormatterPluginExecutor(log *logger.Logger, notify plugin.Notify, dir string, manager *FormatterConfigManager) *FormatterPluginExecutor {
	return plugin.NewPluginExecutor[Formatter](log, notify, dir, NewFormatterAdapter(manager), formatterExecutorMetrics)
}

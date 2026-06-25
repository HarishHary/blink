package formatters

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var formatterExecutorMetrics = plugin.NewPluginExecutorMetrics("formatters")

type FormatterPluginExecutor = plugin.PluginExecutor[Formatter]

func NewFormatterPluginExecutor(log *logger.Logger, notify plugin.Notify, dir string, snap plugin.SnapshotSource, manager *FormatterConfigWatcher) *FormatterPluginExecutor {
	return plugin.NewPluginExecutor[Formatter](log, notify, dir, snap, NewFormatterAdapter(manager), formatterExecutorMetrics)
}

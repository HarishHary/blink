package formatters

import (
	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/logger"
)

var formatterExecutorMetrics = executor.NewPluginExecutorMetrics("formatters")

type FormatterPluginExecutor = executor.PluginExecutor[Formatter]

func NewFormatterPluginExecutor(log *logger.Logger, notify executor.Notify, dir string, manager *FormatterConfigManager) *FormatterPluginExecutor {
	return executor.NewPluginExecutor[Formatter](log, notify, dir, NewFormatterAdapter(manager), formatterExecutorMetrics)
}

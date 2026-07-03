package formatters

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var formatterExecutorMetrics = plugin.NewPluginExecutorMetrics("formatters")

type FormatterPluginExecutor = plugin.PluginExecutor[Formatter]

func NewFormatterPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*FormatterMetadata]) *FormatterPluginExecutor {
	return plugin.NewPluginExecutor[Formatter](logger, notify, dir, src, NewFormatterAdapter(cfg), formatterExecutorMetrics)
}

package formatters

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var formatterExecutorMetrics = plugin.NewPluginExecutorMetrics("formatters")

// PluginExecutor reconciles and runs formatter plugin subprocesses.
type PluginExecutor = plugin.PluginExecutor[Formatter]

// NewPluginExecutor builds the formatter plugin executor.
func NewPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*FormatterMetadata]) *PluginExecutor {
	return plugin.NewPluginExecutor[Formatter](logger, notify, dir, src, NewAdapter(cfg), formatterExecutorMetrics)
}

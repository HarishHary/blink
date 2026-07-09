package matchers

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var matcherExecutorMetrics = plugin.NewPluginExecutorMetrics("matchers")

// PluginExecutor reconciles and runs matcher plugin subprocesses.
type PluginExecutor = plugin.PluginExecutor[Matcher]

// NewPluginExecutor builds the matcher plugin executor.
func NewPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*MatcherMetadata]) *PluginExecutor {
	return plugin.NewPluginExecutor[Matcher](logger, notify, dir, src, NewAdapter(cfg), matcherExecutorMetrics)
}

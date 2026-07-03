package matchers

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var matcherExecutorMetrics = plugin.NewPluginExecutorMetrics("matchers")

type PluginExecutor = plugin.PluginExecutor[Matcher]

func NewPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*MatcherMetadata]) *PluginExecutor {
	return plugin.NewPluginExecutor[Matcher](logger, notify, dir, src, NewAdapter(cfg), matcherExecutorMetrics)
}

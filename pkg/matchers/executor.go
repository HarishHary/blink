package matchers

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var matcherExecutorMetrics = plugin.NewPluginExecutorMetrics("matchers")

type MatcherPluginExecutor = plugin.PluginExecutor[Matcher]

func NewMatcherPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*MatcherMetadata]) *MatcherPluginExecutor {
	return plugin.NewPluginExecutor[Matcher](logger, notify, dir, src, NewMatcherAdapter(cfg), matcherExecutorMetrics)
}

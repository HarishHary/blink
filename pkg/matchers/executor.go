package matchers

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var matcherExecutorMetrics = plugin.NewPluginExecutorMetrics("matchersvc")

type MatcherPluginExecutor = plugin.PluginExecutor[Matcher]

func NewMatcherPluginExecutor(log *logger.Logger, notify plugin.Notify, dir string, snap plugin.SnapshotSource, manager *MatcherConfigWatcher) *MatcherPluginExecutor {
	return plugin.NewPluginExecutor[Matcher](log, notify, dir, snap, NewMatcherAdapter(manager), matcherExecutorMetrics)
}

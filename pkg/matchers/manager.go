package matchers

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var matcherExecutorMetrics = plugin.NewPluginManagerMetrics("matchersvc")

type MatcherPluginExecutor = plugin.PluginExecutor[Matcher]

func NewMatcherPluginExecutor(log *logger.Logger, notify plugin.Notify, dir string, manager *MatcherConfigManager) *MatcherPluginExecutor {
	return plugin.NewPluginExecutor[Matcher](log, notify, dir, NewMatcherAdapter(manager), matcherExecutorMetrics)
}

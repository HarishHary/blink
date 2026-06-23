package matchers

import (
	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/logger"
)

var matcherExecutorMetrics = executor.NewPluginExecutorMetrics("matchersvc")

type MatcherPluginExecutor = executor.PluginExecutor[Matcher]

func NewMatcherPluginExecutor(log *logger.Logger, notify executor.Notify, dir string, manager *MatcherConfigManager) *MatcherPluginExecutor {
	return executor.NewPluginExecutor[Matcher](log, notify, dir, NewMatcherAdapter(manager), matcherExecutorMetrics)
}

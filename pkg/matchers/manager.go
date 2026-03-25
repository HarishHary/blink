package matchers

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var matcherManagerMetrics = plugin.NewPluginManagerMetrics("matchersvc")

type MatcherPluginManager = plugin.PluginManager[Matcher]

func NewMatcherPluginManager(log *logger.Logger, notify plugin.Notify, dir string, manager *MatcherConfigManager) *plugin.PluginManager[Matcher] {
	return plugin.NewPluginManager[Matcher](log, notify, dir, &MatcherAdapter{Manager: manager}, matcherManagerMetrics)
}

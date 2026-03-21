package matchers

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pluginmgr"
)

var matcherManagerMetrics = pluginmgr.NewPluginManagerMetrics("matchersvc")

func NewManager(log *logger.Logger, notify pluginmgr.Notify, dir string) pluginmgr.Plugin {
	return pluginmgr.NewPluginManager[Matcher](log, notify, dir, &MatcherAdapter{}, matcherManagerMetrics)
}

package matchers

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/matchers/config"
)

var matcherManagerMetrics = plugin.NewPluginManagerMetrics("matchersvc")

func NewManager(log *logger.Logger, notify plugin.Notify, dir string, watcher *config.Watcher) *plugin.PluginManager[Matcher] {
	return plugin.NewPluginManager[Matcher](log, notify, dir, &MatcherAdapter{Watcher: watcher}, matcherManagerMetrics)
}

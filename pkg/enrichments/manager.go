package enrichments

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/pluginmgr"
)

var enrichmentManagerMetrics = pluginmgr.NewPluginManagerMetrics("enrichmentsvc")

func NewManager(log *logger.Logger, notify pluginmgr.Notify, dir string) pluginmgr.Plugin {
	return pluginmgr.NewPluginManager[IEnrichment](log, notify, dir, &EnrichmentAdapter{}, enrichmentManagerMetrics)
}

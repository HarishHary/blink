package enrichments

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/pkg/enrichments/config"
)

var enrichmentManagerMetrics = plugin.NewPluginManagerMetrics("enrichmentsvc")

func NewManager(log *logger.Logger, notify plugin.Notify, dir string, watcher *config.Watcher) *plugin.PluginManager[Enrichment] {
	return plugin.NewPluginManager[Enrichment](log, notify, dir, &EnrichmentAdapter{Watcher: watcher}, enrichmentManagerMetrics)
}

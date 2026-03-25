package enrichments

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var enrichmentManagerMetrics = plugin.NewPluginManagerMetrics("enrichmentsvc")

type EnrichmentPluginManager = plugin.PluginManager[Enrichment]

func NewEnrichmentPluginManager(log *logger.Logger, notify plugin.Notify, dir string, manager *EnrichmentConfigManager) *EnrichmentPluginManager {
	return plugin.NewPluginManager[Enrichment](log, notify, dir, &EnrichmentConfigAdapter{Manager: manager}, enrichmentManagerMetrics)
}

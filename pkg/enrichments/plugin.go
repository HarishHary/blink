package enrichments

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var enrichmentExecutorMetrics = plugin.NewPluginManagerMetrics("enrichmentsvc")

type EnrichmentPluginExecutor = plugin.PluginExecutor[Enrichment]

func NewEnrichmentPluginExecutor(log *logger.Logger, notify plugin.Notify, dir string, manager *EnrichmentConfigManager) *EnrichmentPluginExecutor {
	return plugin.NewPluginExecutor[Enrichment](log, notify, dir, NewEnrichmentAdapter(manager), enrichmentExecutorMetrics)
}

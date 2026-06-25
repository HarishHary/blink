package enrichments

import (
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var enrichmentExecutorMetrics = plugin.NewPluginExecutorMetrics("enrichmentsvc")

type EnrichmentPluginExecutor = plugin.PluginExecutor[Enrichment]

func NewEnrichmentPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, snap plugin.SnapshotSource, manager *EnrichmentConfigWatcher) *EnrichmentPluginExecutor {
	return plugin.NewPluginExecutor[Enrichment](logger, notify, dir, snap, NewEnrichmentAdapter(manager), enrichmentExecutorMetrics)
}

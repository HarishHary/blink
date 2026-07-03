package enrichments

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var enrichmentExecutorMetrics = plugin.NewPluginExecutorMetrics("enrichment")

type EnrichmentPluginExecutor = plugin.PluginExecutor[Enrichment]

func NewEnrichmentPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*EnrichmentMetadata]) *EnrichmentPluginExecutor {
	return plugin.NewPluginExecutor[Enrichment](logger, notify, dir, src, NewEnrichmentAdapter(cfg), enrichmentExecutorMetrics)
}

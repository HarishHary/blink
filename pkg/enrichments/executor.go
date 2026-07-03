package enrichments

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var enrichmentExecutorMetrics = plugin.NewPluginExecutorMetrics("enrichment")

type PluginExecutor = plugin.PluginExecutor[Enrichment]

func NewPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*EnrichmentMetadata]) *PluginExecutor {
	return plugin.NewPluginExecutor[Enrichment](logger, notify, dir, src, NewAdapter(cfg), enrichmentExecutorMetrics)
}

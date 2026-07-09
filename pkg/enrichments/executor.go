package enrichments

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var enrichmentExecutorMetrics = plugin.NewPluginExecutorMetrics("enrichment")

// PluginExecutor reconciles and runs enrichment plugin subprocesses.
type PluginExecutor = plugin.PluginExecutor[Enrichment]

// NewPluginExecutor builds the enrichment plugin executor.
func NewPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*EnrichmentMetadata]) *PluginExecutor {
	return plugin.NewPluginExecutor[Enrichment](logger, notify, dir, src, NewAdapter(cfg), enrichmentExecutorMetrics)
}

package enrichments

import (
	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/logger"
)

var enrichmentExecutorMetrics = executor.NewPluginExecutorMetrics("enrichmentsvc")

type EnrichmentPluginExecutor = executor.PluginExecutor[Enrichment]

func NewEnrichmentPluginExecutor(logger *logger.Logger, notify executor.Notify, dir string, manager *EnrichmentConfigManager) *EnrichmentPluginExecutor {
	return executor.NewPluginExecutor[Enrichment](logger, notify, dir, NewEnrichmentAdapter(manager), enrichmentExecutorMetrics)
}

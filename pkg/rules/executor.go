package rules

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var ruleExecutorMetrics = plugin.NewPluginExecutorMetrics("rules")

// PluginExecutor aliases the generic plugin executor for rules.
type PluginExecutor = plugin.PluginExecutor[Rule]

// NewPluginExecutor builds a rules plugin executor.
func NewPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*RuleMetadata]) *PluginExecutor {
	return plugin.NewPluginExecutor[Rule](logger, notify, dir, src, NewAdapter(cfg), ruleExecutorMetrics)
}

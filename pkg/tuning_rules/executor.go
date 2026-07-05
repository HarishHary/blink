package tuning_rules

import (
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
)

var tuningExecutorMetrics = plugin.NewPluginExecutorMetrics("tuning_rules")

// PluginExecutor is the tuning-rule instantiation of plugin.PluginExecutor.
type PluginExecutor = plugin.PluginExecutor[TuningRule]

// NewPluginExecutor builds the tuning-rule plugin executor reconciling subprocesses from src.
func NewPluginExecutor(logger *logger.Logger, notify plugin.Notify, dir string, src plugin.SnapshotSource, cfg config.Source[*TuningRuleMetadata]) *PluginExecutor {
	return plugin.NewPluginExecutor[TuningRule](logger, notify, dir, src, NewAdapter(cfg), tuningExecutorMetrics)
}

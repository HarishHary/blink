package plugin

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PluginExecutorMetrics holds the Prometheus metrics shared by all plugin executors.
type PluginExecutorMetrics struct {
	Starts             prometheus.Counter
	Crashes            prometheus.Counter
	Restarts           prometheus.Counter
	Updates            prometheus.Counter
	DigestMismatches   prometheus.Counter
	StartLatency       prometheus.Histogram
	ActiveSubprocesses *prometheus.GaugeVec
}

// NewPluginExecutorMetrics registers and returns a metric set for the given subsystem.
func NewPluginExecutorMetrics(subsystem string) *PluginExecutorMetrics {
	return &PluginExecutorMetrics{
		Starts: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "blink", Subsystem: "plugin_executor" + subsystem, Name: "plugin_starts_total",
			Help: "Total plugin subprocess starts.",
		}),
		Crashes: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "blink", Subsystem: "plugin_executor" + subsystem, Name: "plugin_crashes_total",
			Help: "Total plugin subprocess crashes detected by ping loop.",
		}),
		Restarts: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "blink", Subsystem: "plugin_executor" + subsystem, Name: "plugin_restarts_total",
			Help: "Total plugin subprocess restarts after crash.",
		}),
		Updates: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "blink", Subsystem: "plugin_executor" + subsystem, Name: "plugin_updates_total",
			Help: "Total plugin subprocess hot-updates (binary replacement).",
		}),
		DigestMismatches: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "blink", Subsystem: "plugin_executor" + subsystem, Name: "plugin_digest_mismatches_total",
			Help: "Local binaries whose checksum did not match the digest published in the snapshot (refused to run).",
		}),
		StartLatency: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: "blink", Subsystem: "plugin_executor" + subsystem, Name: "plugin_start_latency_seconds",
			Help:    "Time from plugin launch start to first bus publish.",
			Buckets: prometheus.DefBuckets,
		}),
		ActiveSubprocesses: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "blink", Subsystem: "plugin_executor" + subsystem, Name: "plugin_active_subprocesses",
			Help: "Number of currently active plugin subprocesses.",
		}, []string{"type"}),
	}
}

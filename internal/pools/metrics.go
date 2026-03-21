package pools

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Holds the Prometheus metrics for one ProcessPool instance.
type PoolMetrics struct {
	poolSize      *prometheus.GaugeVec
	poolInflight  *prometheus.GaugeVec
	drainDuration *prometheus.HistogramVec
	killSwitches  *prometheus.CounterVec
	shadowDiffs   *prometheus.CounterVec
}

// Registers and returns Prometheus metrics namespaced under
func NewPoolMetrics(subsystem string) *PoolMetrics {
	return &PoolMetrics{
		poolSize: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "blink", Subsystem: "pool_" + subsystem,
			Name: "pool_size", Help: "Active subprocess count per pool.",
		}, []string{"plugin_id", "version"}),
		poolInflight: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "blink", Subsystem: "pool_" + subsystem,
			Name: "pool_inflight", Help: "Evaluations currently in-flight per pool.",
		}, []string{"plugin_id", "version"}),
		drainDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "blink", Subsystem: "pool_" + subsystem,
			Name: "drain_duration_seconds", Help: "Time to drain an old pool.",
			Buckets: prometheus.DefBuckets,
		}, []string{"plugin_id", "version"}),
		killSwitches: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "blink", Subsystem: "pool_" + subsystem,
			Name: "kill_switch_total", Help: "Kill switch activations.",
		}, []string{"plugin_id"}),
		shadowDiffs: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "blink", Subsystem: "pool_" + subsystem,
			Name: "shadow_diff_total", Help: "Shadow evaluation errors or divergences.",
		}, []string{"plugin_id"}),
	}
}

package services

import (
	"net/http"

	"github.com/harishhary/blink/internal/logger"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServeHealth starts the blocking metrics, liveness, and readiness HTTP server.
func ServeHealth(logger *logger.Logger, addr string, readyFn func() bool) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if readyFn == nil || readyFn() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.FatalF("health server failed: %v", err)
	}
}

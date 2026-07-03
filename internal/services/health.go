package services

import (
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServeHealth starts the observability HTTP server (blocking - run it in a goroutine).
// It serves /metrics, /health/live (always 200), and /health/ready.
//
// /health/ready returns 503 until ready() is true, then 200. Pass nil for services with no
// readiness dependency (always ready). Pipeline pods pass their SnapshotReader.Ready so they
// stay out of rotation until the control plane has delivered a snapshot - a cold-started pod
// with no desired state shouldn't receive traffic.
func ServeHealth(addr string, ready func() bool) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if ready == nil || ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	log.Fatal(http.ListenAndServe(addr, mux))
}

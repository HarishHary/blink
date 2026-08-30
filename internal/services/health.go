package services

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net"
	"net/http"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const healthShutdownTimeout = 5 * time.Second

// HealthService serves metrics, liveness, and readiness until its context is cancelled.
type HealthService struct {
	addr     string
	readyFn  func() bool
	statusFn func() any
}

func NewHealthService(addr string, readyFn func() bool, statusFn func() any) *HealthService {
	return &HealthService{addr: addr, readyFn: readyFn, statusFn: statusFn}
}

func (s *HealthService) Name() string { return "health service" }

func (s *HealthService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if s.readyFn == nil || s.readyFn() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if s.statusFn != nil {
		mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.statusFn())
		})
	}
	return mux
}

func (s *HealthService) Run(ctx context.Context) errors.Error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return errors.NewE(err)
	}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()

	select {
	case err := <-done:
		if err == nil || stderrors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.NewE(err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), healthShutdownTimeout)
		err := server.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			_ = server.Close()
			return errors.NewE(err)
		}
		if err := <-done; err != nil && !stderrors.Is(err, http.ErrServerClosed) {
			return errors.NewE(err)
		}
		return nil
	}
}

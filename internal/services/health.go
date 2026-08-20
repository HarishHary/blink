package services

import (
	"context"
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
	addr    string
	readyFn func() bool
}

func NewHealthService(addr string, readyFn func() bool) *HealthService {
	return &HealthService{addr: addr, readyFn: readyFn}
}

func (s *HealthService) Name() string { return "health" }

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
	return mux
}

func (s *HealthService) Run(ctx context.Context) errors.Error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return errors.NewE(err)
	}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
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

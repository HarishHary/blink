package services

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	serviceRestarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "blink",
		Subsystem: "runner",
		Name:      "service_restarts_total",
		Help:      "Total number of times a service has been restarted after failure.",
	}, []string{"service"})

	serviceRestartDelay = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "blink",
		Subsystem: "runner",
		Name:      "service_restart_delay_seconds",
		Help:      "Delay before restarting a failed service.",
		Buckets:   []float64{1, 2, 4, 8, 16, 32, 60},
	}, []string{"service"})
)

const (
	backoffBase = time.Second
	backoffMax  = 60 * time.Second
)

type Runner struct {
	inits    []service
	services []service
	logger   *logger.Logger
}

func New(logger *logger.Logger) *Runner {
	return &Runner{
		services: make([]service, 0),
		logger:   logger,
	}
}

func (r *Runner) RegisterInit(services ...service) {
	r.inits = append(r.inits, services...)
}

func (r *Runner) Register(services ...service) {
	r.services = append(r.services, services...)
}

// Run executes init services, then runs regular services until cancellation and waits for shutdown.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range r.inits {
		wg.Add(1)
		svc := r.inits[i]
		go func() {
			defer wg.Done()
			r.logger.Info("init service %s started", svc.Name())
			if err := svc.Run(ctx); err != nil {
				r.logger.ErrorF("init service %s terminated with error: %s", svc.Name(), err)
			} else {
				r.logger.Info("init service %s completed", svc.Name())
			}
		}()
	}
	wg.Wait()

	for _, svc := range r.services {
		wg.Go(func() { r.runWithBackoff(ctx, svc) })
	}

	<-ctx.Done()
	r.logger.Info("context cancelled; waiting for services to stop")
	wg.Wait()
	r.logger.Info("all services stopped")
}

// runWithBackoff runs a service with exponential backoff on failure,
// stopping when ctx is cancelled.
func (r *Runner) runWithBackoff(ctx context.Context, svc service) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			r.logger.Info("service %s stopped (context cancelled)", svc.Name())
			return
		}
		r.logger.Info("service %s starting (attempt %d)", svc.Name(), attempt+1)
		if err := svc.Run(ctx); err != nil {
			r.logger.ErrorF("service %s error: %s", svc.Name(), err)
		}

		if ctx.Err() != nil {
			r.logger.Info("service %s stopped (context cancelled)", svc.Name())
			return
		}

		serviceRestarts.WithLabelValues(svc.Name()).Inc()
		attempt++

		// Exponential backoff: base * 2^(attempt-1), capped at backoffMax, with ±25% jitter.
		exp := math.Min(float64(backoffBase)*math.Pow(2, float64(attempt-1)), float64(backoffMax))
		jitter := time.Duration(rand.Int63n(int64(exp / 4)))
		delay := time.Duration(exp) + jitter
		serviceRestartDelay.WithLabelValues(svc.Name()).Observe(delay.Seconds())
		r.logger.Info("service %s restarting in %v", svc.Name(), delay.Round(time.Millisecond))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			r.logger.Info("service %s restart cancelled (context cancelled)", svc.Name())
			return
		}
	}
}

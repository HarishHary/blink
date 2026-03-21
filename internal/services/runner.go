package services

import (
	"context"
	"log"
	"math"
	"math/rand"
	"os"
	"sync"
	"time"

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
	inits    []Service
	services []Service
	logger   *log.Logger
}

func New() *Runner {
	return &Runner{
		services: make([]Service, 0),
		logger:   log.New(os.Stdout, "[SERVICE - RUNNER] ", log.Ldate|log.Ltime),
	}
}

func (r *Runner) RegisterInit(services ...Service) {
	r.inits = append(r.inits, services...)
}

func (r *Runner) Register(services ...Service) {
	r.services = append(r.services, services...)
}

// Run executes all registered services. Init services run first to completion,
// then regular services are started concurrently with auto-restart on failure.
// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range r.inits {
		wg.Add(1)
		svc := r.inits[i]
		go func() {
			defer wg.Done()
			r.logger.Printf("init service %s started\n", svc.Name())
			if err := svc.Run(ctx); err != nil { //nolint:staticcheck
				r.logger.Printf("init service %s terminated with error: %s\n", svc.Name(), err)
			} else {
				r.logger.Printf("init service %s completed\n", svc.Name())
			}
		}()
	}
	wg.Wait()

	for _, svc := range r.services {
		go r.runWithBackoff(ctx, svc)
	}

	<-ctx.Done()
	r.logger.Println("runner: context cancelled, shutting down")
}

// runWithBackoff runs a service with exponential backoff on failure,
// stopping when ctx is cancelled.
func (r *Runner) runWithBackoff(ctx context.Context, svc Service) {
	attempt := 0
	for {
		r.logger.Printf("service %s starting (attempt %d)\n", svc.Name(), attempt+1)
		if err := svc.Run(ctx); err != nil {
			r.logger.Printf("service %s error: %s\n", svc.Name(), err)
		}

		if ctx.Err() != nil {
			r.logger.Printf("service %s stopped (context cancelled)\n", svc.Name())
			return
		}

		serviceRestarts.WithLabelValues(svc.Name()).Inc()
		attempt++

		// Exponential backoff: base * 2^(attempt-1), capped at backoffMax, with ±25% jitter.
		exp := math.Min(float64(backoffBase)*math.Pow(2, float64(attempt-1)), float64(backoffMax))
		jitter := time.Duration(rand.Int63n(int64(exp / 4)))
		delay := time.Duration(exp) + jitter
		serviceRestartDelay.WithLabelValues(svc.Name()).Observe(delay.Seconds())
		r.logger.Printf("service %s restarting in %v\n", svc.Name(), delay.Round(time.Millisecond))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			r.logger.Printf("service %s restart cancelled (context cancelled)\n", svc.Name())
			return
		}
	}
}

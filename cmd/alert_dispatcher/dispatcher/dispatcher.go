package dispatcher

import (
	"context"
	"time"

	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dispatchers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	alertsIn         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_dispatcher", Name: "alerts_in_total"})
	alertsOut        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_dispatcher", Name: "alerts_out_total"})
	alertsDispatched = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_dispatcher", Name: "alerts_dispatched_total"}, []string{"dispatcher"})
	dispatchErrors   = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_dispatcher", Name: "dispatch_errors_total"}, []string{"dispatcher"})
	dispatchLatency  = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "alert_dispatcher", Name: "dispatch_latency_seconds", Buckets: prometheus.DefBuckets}, []string{"dispatcher"})
	parseErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_dispatcher", Name: "parse_errors_total"})
)

type DispatcherService struct {
	*logger.Logger
	reader         brokers.Reader
	dispatcherRepo *dispatchers.DispatcherRepository
}

// Config is the explicit set of dependencies New needs, injected by main.
type Config struct {
	Broker          brokers.Broker
	DispatcherTopic string
	DispatcherGroup string
}

func New(c Config, dispatcherRepo *dispatchers.DispatcherRepository) *DispatcherService {
	return &DispatcherService{
		Logger:         logger.New("alert-dispatcher", "dev"),
		reader:         c.Broker.NewReader(c.DispatcherTopic, c.DispatcherGroup),
		dispatcherRepo: dispatcherRepo,
	}
}

func (service *DispatcherService) Name() string { return "alert-dispatcher" }

func (service *DispatcherService) Run(ctx context.Context) errors.Error {
	for {
		msgs, err := service.reader.ReadBatch(ctx, 50)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			service.Error(errors.NewE(err))
			continue
		}

		for _, m := range msgs {
			alert, err := alerts.Unmarshal(m.Value)
			if err != nil {
				parseErrors.Inc()
				service.Error(errors.NewE(err))
				continue
			}
			alertsIn.Inc()
			service.Info("dispatching alert %s", alert.Id)

			for _, name := range alert.Rule.Dispatchers {
				disp, derr := service.dispatcherRepo.GetDispatcher(name)
				if derr != nil {
					service.Error(derr)
					continue
				}
				start := time.Now()
				sent, derr := disp.Dispatch(*alert)
				dispatchLatency.WithLabelValues(disp.Name()).Observe(time.Since(start).Seconds())
				if derr != nil {
					dispatchErrors.WithLabelValues(disp.Name()).Inc()
					service.Error(derr)
					continue
				}
				if sent {
					alertsDispatched.WithLabelValues(disp.Name()).Inc()
				} else {
					service.Info("dispatcher %s returned false for alert %s", disp.Name(), alert.Id)
				}
			}
			alertsOut.Inc()
		}

		if err := service.reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			service.Error(errors.NewE(err))
		}
	}
}

package dispatcher

import (
	"context"
	"log"
	"time"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	svcctx "github.com/harishhary/blink/internal/context"
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
	svcctx.ServiceContext
	reader         broker.Reader
	dispatcherRepo *dispatchers.DispatcherRepository
}

func New(dispatcherRepo *dispatchers.DispatcherRepository) (*DispatcherService, error) {
	serviceContext := svcctx.New("BLINK-ALERT-DISPATCHER - DISPATCH")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	cfg := serviceContext.Configuration()
	b := kafka.NewKafkaBroker(cfg.Kafka)
	reader := b.NewReader(cfg.Topics.DispatcherTopic, cfg.Topics.DispatcherGroup)

	return &DispatcherService{
		ServiceContext: serviceContext,
		reader:         reader,
		dispatcherRepo: dispatcherRepo,
	}, nil
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
			service.Info("dispatching alert %s", alert.AlertID)

			for _, name := range alert.Rule.Dispatchers() {
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
					log.Printf("dispatcher %s returned false for alert %s", disp.Name(), alert.AlertID)
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

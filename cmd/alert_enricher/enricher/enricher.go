package enricher

import (
	"context"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/alerts"
	enrichcatalog "github.com/harishhary/blink/pkg/enrichments/pool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const defaultEnrichmentTimeout = 5 * time.Second

var (
	alertsIn           = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "alerts_in_total"})
	alertsOut          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "alerts_out_total"})
	alertsDLQ          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "alerts_dlq_total"})
	enrichmentsApplied = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "enrichments_applied_total"}, []string{"enrichment"})
	enrichmentErrors   = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "enrichment_errors_total"}, []string{"enrichment"})
	enrichmentLatency  = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "enrichment_latency_seconds", Buckets: prometheus.DefBuckets}, []string{"enrichment"})
	parseErrors        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "parse_errors_total"})
	writeErrors        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "write_errors_total"})
)

// enrichAlertState holds a decoded alert and its enrichment outcome for a batch entry.
type enrichAlertState struct {
	key        []byte
	alert      *alerts.Alert
	anyMissing bool
}

// EnricherService reads alerts from Kafka, enriches them, and writes to the formatter topic.
type EnricherService struct {
	svcctx.ServiceContext
	reader broker.Reader
	writer broker.Writer
	dlq    broker.Writer
	pool   *enrichcatalog.Pool
}

func NewEnricherService(pool *enrichcatalog.Pool) (*EnricherService, error) {
	serviceContext := svcctx.New("BLINK-ALERT-ENRICHER - ENRICH")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	cfg := serviceContext.Configuration()
	b := kafka.NewKafkaBroker(cfg.Kafka)
	reader := b.NewReader(cfg.Topics.EnricherTopic, cfg.Topics.EnricherGroup)
	writer := b.NewWriter(cfg.Topics.FormatterTopic)

	var dlq broker.Writer
	if cfg.Topics.EnricherDLQTopic != "" {
		dlq = b.NewWriter(cfg.Topics.EnricherDLQTopic)
	}

	return &EnricherService{
		ServiceContext: serviceContext,
		reader:         reader,
		writer:         writer,
		dlq:            dlq,
		pool:           pool,
	}, nil
}

func (service *EnricherService) Name() string { return "alert-enricher" }

func (service *EnricherService) Run(ctx context.Context) errors.Error {
	for {
		msgs, err := service.reader.ReadBatch(ctx, 50)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			service.Error(errors.NewE(err))
			continue
		}

		service.processBatch(ctx, msgs)

		if err := service.reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			service.Error(errors.NewE(err))
		}
	}
}

func (service *EnricherService) processBatch(ctx context.Context, msgs []broker.Message) {
	// Decode all alerts.
	states := make([]*enrichAlertState, 0, len(msgs))
	for _, m := range msgs {
		alert, err := alerts.Unmarshal(m.Value)
		if err != nil {
			parseErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		alertsIn.Inc()
		states = append(states, &enrichAlertState{key: m.Key, alert: alert})
	}
	if len(states) == 0 {
		return
	}

	// Group by enrichment name: name → indices into states.
	// Respect already-applied enrichments from prior DLQ retries.
	byEnrichment := make(map[string][]int)
	for i, s := range states {
		applied := make(map[string]struct{}, len(s.alert.EnrichmentsApplied))
		for _, name := range s.alert.EnrichmentsApplied {
			applied[name] = struct{}{}
		}
		for _, name := range s.alert.Rule.Enrichments() {
			if _, done := applied[name]; done {
				continue
			}
			byEnrichment[name] = append(byEnrichment[name], i)
		}
	}

	// Fan out: one goroutine per enrichment with all its alerts.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, idxs := range byEnrichment {
		wg.Add(1)
		go func(name string, idxs []int) {
			defer wg.Done()

			alerts := make([]*alerts.Alert, len(idxs))
			for j, idx := range idxs {
				alerts[j] = states[idx].alert
			}

			cctx, cancel := context.WithTimeout(ctx, defaultEnrichmentTimeout)
			defer cancel()
			start := time.Now()
			absent, removed, errs := service.pool.Enrich(cctx, name, alerts, "")
			enrichmentLatency.WithLabelValues(name).Observe(time.Since(start).Seconds())

			mu.Lock()
			defer mu.Unlock()
			switch {
			case removed:
				service.Error(errors.NewF("enrichment %s removed", name))
				for _, idx := range idxs {
					states[idx].anyMissing = true
				}
			case absent:
				service.Error(errors.NewF("enrichment %s not found", name))
				for _, idx := range idxs {
					states[idx].anyMissing = true
				}
			default:
				for j, idx := range idxs {
					if errs[j] != nil {
						enrichmentErrors.WithLabelValues(name).Inc()
						service.Error(errs[j])
					} else {
						enrichmentsApplied.WithLabelValues(name).Inc()
						states[idx].alert.EnrichmentsApplied = append(states[idx].alert.EnrichmentsApplied, name)
					}
				}
			}
		}(name, idxs)
	}
	wg.Wait()

	// Write results.
	for _, s := range states {
		if s.anyMissing {
			s.alert.Attempts++
			if s.alert.Attempts >= services.MaxPluginAttempts || service.dlq == nil {
				service.Info("alert %s passed through after %d attempts (enrichment unavailable)", s.alert.AlertID, s.alert.Attempts)
				s.alert.EnrichmentsApplied = nil
				// fall through to write
			} else {
				payload, err := alerts.Marshal(s.alert)
				if err != nil {
					writeErrors.Inc()
					service.Error(errors.NewE(err))
					continue
				}
				err = service.dlq.WriteMessages(ctx, broker.Message{Key: s.key, Value: payload})
				if err != nil {
					writeErrors.Inc()
					service.Error(errors.NewE(err))
				} else {
					alertsDLQ.Inc()
				}
				continue
			}
		}

		s.alert.EnrichmentsApplied = nil
		payload, err := alerts.Marshal(s.alert)
		if err != nil {
			writeErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		err = service.writer.WriteMessages(ctx, broker.Message{Key: s.key, Value: payload})
		if err != nil {
			writeErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		alertsOut.Inc()
	}
}

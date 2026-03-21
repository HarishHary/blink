package enricher

import (
	"context"
	"sync"
	"sync/atomic"
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

const (
	defaultEnrichmentTimeout = 5 * time.Second
)

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

// Reads alerts from Kafka, applies enrichments declared by the alert's rule, and writes to the formatter topic.
func (service *EnricherService) Run(ctx context.Context) errors.Error {
	return services.RunAlertPipeline(ctx, service.Logger, service.reader, service.writer, service.dlq, 50,
		services.PipelineCounters{
			In: alertsIn.Inc, Out: alertsOut.Inc, DLQ: alertsDLQ.Inc,
			ParseError: parseErrors.Inc, WriteError: writeErrors.Inc,
		},
		func(ctx context.Context, _ []byte, alert *alerts.Alert) (skip bool, deadLetter bool) {
			service.Info("enriching alert %s", alert.AlertID)

			applied := make(map[string]struct{}, len(alert.EnrichmentsApplied))
			for _, name := range alert.EnrichmentsApplied {
				applied[name] = struct{}{}
			}

			var (
				anyMissing atomic.Bool
				mu         sync.Mutex
				succeeded  []string
				wg         sync.WaitGroup
			)
			for _, name := range alert.Rule.Enrichments() {
				if _, done := applied[name]; done {
					continue
				}
				wg.Add(1)
				go func(enrName string) {
					defer wg.Done()

					cctx, cancel := context.WithTimeout(ctx, defaultEnrichmentTimeout)
					defer cancel()
					start := time.Now()
					absent, removed, err := service.pool.Enrich(cctx, enrName, alert, "")
					switch {
					case removed:
						anyMissing.Store(true)
						service.Error(errors.NewF("enrichment %s removed - alert %s missing enrichment", enrName, alert.AlertID))
					case absent:
						anyMissing.Store(true)
						service.Error(errors.NewF("enrichment %s not found - alert %s missing enrichment", enrName, alert.AlertID))
					case err != nil:
						enrichmentErrors.WithLabelValues(enrName).Inc()
						service.Error(errors.NewF("enrichment %s failed: %v", enrName, err))
					default:
						enrichmentsApplied.WithLabelValues(enrName).Inc()
						mu.Lock()
						succeeded = append(succeeded, enrName)
						mu.Unlock()
					}
					enrichmentLatency.WithLabelValues(enrName).Observe(time.Since(start).Seconds())
				}(name)
			}
			wg.Wait()

			alert.EnrichmentsApplied = append(alert.EnrichmentsApplied, succeeded...)

			if anyMissing.Load() {
				alert.Attempts++
				if alert.Attempts >= services.MaxPluginAttempts {
					service.Info("alert %s passed through after %d attempts (enrichment unavailable)", alert.AlertID, alert.Attempts)
					alert.EnrichmentsApplied = nil
					return false, false
				}
				return false, true
			}
			alert.EnrichmentsApplied = nil
			return false, false
		},
	)
}

package enricher

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments"
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

// alertState holds a decoded alert and its enrichment outcome for a batch entry.
type alertState struct {
	key        []byte
	alert      *alerts.Alert
	deadLetter bool
}

// EnricherService reads alerts from Kafka, enriches them, and writes to the formatter topic.
type EnricherService struct {
	*logger.Logger
	reader brokers.Reader
	writer brokers.Writer
	dlq    brokers.Writer
	pool   *enrichments.Pool
}

// Config is the explicit set of dependencies NewEnricherService needs, injected by main.
// Config's topic fields are populated from the environment by main (which embeds it);
// Broker is injected after load.
type Config struct {
	Broker         brokers.Broker
	EnricherTopic  string `env:"KAFKA_TOPIC_ENRICHER"`
	EnricherGroup  string `env:"KAFKA_GROUP_ENRICHER"`
	FormatterTopic string `env:"KAFKA_TOPIC_FORMATTER"`
	DLQTopic       string `env:"KAFKA_TOPIC_ENRICHER_DLQ,optional"` // optional; empty = no dead-letter queue
}

func NewEnricherService(c Config, pool *enrichments.Pool) *EnricherService {
	var dlq brokers.Writer
	if c.DLQTopic != "" {
		dlq = c.Broker.NewWriter(c.DLQTopic)
	}

	return &EnricherService{
		Logger: logger.New("alert-enricher", "dev"),
		reader: c.Broker.NewReader(c.EnricherTopic, c.EnricherGroup),
		writer: c.Broker.NewWriter(c.FormatterTopic),
		dlq:    dlq,
		pool:   pool,
	}
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

func (service *EnricherService) processBatch(ctx context.Context, msgs []brokers.Message) {
	// Decode all alerts.
	states := make([]*alertState, 0, len(msgs))
	for _, m := range msgs {
		alert, err := alerts.Unmarshal(m.Value)
		if err != nil {
			parseErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		alertsIn.Inc()
		states = append(states, &alertState{key: m.Key, alert: alert})
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
		for _, name := range s.alert.Rule.Enrichments {
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

			// Copy under the lock (a bare read of the shared alert would race the locked writers
			// below); clone Event because Enrich writes into it. Results merge back serially.
			mu.Lock()
			batch := make([]*alerts.Alert, len(idxs))
			for j, idx := range idxs {
				cp := *states[idx].alert
				cp.Event = maps.Clone(cp.Event)
				batch[j] = &cp
			}
			mu.Unlock()

			cctx, cancel := context.WithTimeout(ctx, defaultEnrichmentTimeout)
			defer cancel()
			start := time.Now()
			absent, removed, errs := service.pool.Enrich(cctx, name, batch, "")
			enrichmentLatency.WithLabelValues(name).Observe(time.Since(start).Seconds())

			mu.Lock()
			defer mu.Unlock()
			switch {
			case removed:
				service.Error(errors.NewF("enrichment %s removed", name))
				for _, idx := range idxs {
					states[idx].deadLetter = true
				}
			case absent:
				service.Error(errors.NewF("enrichment %s not found", name))
				for _, idx := range idxs {
					states[idx].deadLetter = true
				}
			default:
				for j, idx := range idxs {
					if errs[j] != nil {
						enrichmentErrors.WithLabelValues(name).Inc()
						service.Error(errs[j])
					} else {
						enrichmentsApplied.WithLabelValues(name).Inc()
						maps.Copy(states[idx].alert.Event, batch[j].Event)
						states[idx].alert.EnrichmentsApplied = append(states[idx].alert.EnrichmentsApplied, name)
					}
				}
			}
		}(name, idxs)
	}
	wg.Wait()

	// Write results.
	for _, s := range states {
		if s.deadLetter {
			s.alert.Attempts++
			if s.alert.Attempts >= services.MaxPluginAttempts || service.dlq == nil {
				service.Info("alert %s passed through after %d attempts (enrichment unavailable)", s.alert.Id, s.alert.Attempts)
				s.alert.EnrichmentsApplied = nil
				// fall through to write
			} else {
				payload, err := alerts.Marshal(s.alert)
				if err != nil {
					writeErrors.Inc()
					service.Error(errors.NewE(err))
					continue
				}
				err = service.dlq.WriteMessages(ctx, brokers.Message{Key: s.key, Value: payload})
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
		err = service.writer.WriteMessages(ctx, brokers.Message{Key: s.key, Value: payload})
		if err != nil {
			writeErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		alertsOut.Inc()
	}
}

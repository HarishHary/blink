package formatter

import (
	"context"
	"sync"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/alerts"
	fmtcatalog "github.com/harishhary/blink/pkg/formatters/pool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	alertsIn          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "alerts_in_total"})
	alertsOut         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "alerts_out_total"})
	alertsDLQ         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "alerts_dlq_total"})
	formattersApplied = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "formatters_applied_total"}, []string{"formatter"})
	formatterErrors   = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "formatter_errors_total"}, []string{"formatter"})
	parseErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "parse_errors_total"})
	writeErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "write_errors_total"})
)

// alertState holds a decoded alert and its formatting outcome.
type alertState struct {
	key        []byte
	alert      *alerts.Alert
	snapshot   []byte // pre-format serialization for rollback on error
	deadLetter bool
}

type FormatterService struct {
	svcctx.ServiceContext
	reader broker.Reader
	writer broker.Writer
	dlq    broker.Writer
	pool   *fmtcatalog.Pool
}

func NewFormatterService(pool *fmtcatalog.Pool) (*FormatterService, error) {
	serviceContext := svcctx.New("BLINK-ALERT-FORMATTER - FORMAT")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	cfg := serviceContext.Configuration()
	b := kafka.NewKafkaBroker(cfg.Kafka)
	reader := b.NewReader(cfg.Topics.FormatterTopic, cfg.Topics.FormatterGroup)
	writer := b.NewWriter(cfg.Topics.DispatcherTopic)

	var dlq broker.Writer
	if cfg.Topics.FormatterDLQTopic != "" {
		dlq = b.NewWriter(cfg.Topics.FormatterDLQTopic)
	}

	return &FormatterService{
		ServiceContext: serviceContext,
		reader:         reader,
		writer:         writer,
		dlq:            dlq,
		pool:           pool,
	}, nil
}

func (service *FormatterService) Name() string { return "alert-formatter" }

func (service *FormatterService) Run(ctx context.Context) errors.Error {
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

func (service *FormatterService) processBatch(ctx context.Context, msgs []broker.Message) {
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
		snapshot, _ := alerts.Marshal(alert)
		states = append(states, &alertState{key: m.Key, alert: alert, snapshot: snapshot})
	}
	if len(states) == 0 {
		return
	}

	// Formatters are applied sequentially per alert but alerts sharing the same
	// formatter at each stage are batched together into one pool call.
	//
	// We collect all unique formatter names across all alerts, then process them
	// stage by stage. Because different rules may declare different formatter
	// sequences, we track per-alert progress via a pointer into their formatter list.
	fmtProgress := make([]int, len(states)) // current formatter index per alert

	// Determine the maximum number of formatter stages across all alerts.
	maxStages := 0
	for _, s := range states {
		if n := len(s.alert.Rule.Formatters()); n > maxStages {
			maxStages = n
		}
	}

	for stage := 0; stage < maxStages; stage++ {
		// Collect alerts at this stage: those whose next formatter is at index `stage`.
		type stageItem struct {
			stateIdx int
			fmtName  string
		}
		// Group by formatter name for this stage.
		byFormatter := make(map[string][]int) // fmtName → stateIdxs
		for i, s := range states {
			if s.deadLetter {
				continue
			}
			fmts := s.alert.Rule.Formatters()
			if fmtProgress[i] >= len(fmts) {
				continue
			}
			name := fmts[fmtProgress[i]]
			byFormatter[name] = append(byFormatter[name], i)
		}
		if len(byFormatter) == 0 {
			break
		}

		// Call each formatter once with all its alerts at this stage.
		var mu sync.Mutex
		var wg sync.WaitGroup
		for name, idxs := range byFormatter {
			wg.Add(1)
			go func(name string, idxs []int) {
				defer wg.Done()

				batch := make([]*alerts.Alert, len(idxs))
				for j, idx := range idxs {
					batch[j] = states[idx].alert
				}

				_, absent, removed, errs := service.pool.Format(ctx, name, batch, "")

				mu.Lock()
				defer mu.Unlock()
				switch {
				case absent, removed:
					label := "not found"
					if removed {
						label = "removed"
					}
					for _, idx := range idxs {
						s := states[idx]
						service.Error(errors.NewF("formatter %s %s - alert %s missing formatter", name, label, s.alert.AlertID))
						s.alert.Attempts++
						if s.alert.Attempts >= services.MaxPluginAttempts {
							service.Info("alert %s passed through after %d attempts (formatter unavailable)", s.alert.AlertID, s.alert.Attempts)
							fmtProgress[idx] = len(s.alert.Rule.Formatters())
						} else {
							s.deadLetter = true
						}
					}
				default:
					for j, idx := range idxs {
						s := states[idx]
						if errs[j] != nil {
							formatterErrors.WithLabelValues(name).Inc()
							service.Error(errs[j])
							// Rollback to pre-format state before DLQ retry.
							if restored, uerr := alerts.Unmarshal(s.snapshot); uerr == nil {
								*s.alert = *restored
							}
							s.alert.Attempts++
							if s.alert.Attempts >= services.MaxPluginAttempts {
								service.Info("alert %s passed through after %d attempts (formatter %s errored)", s.alert.AlertID, s.alert.Attempts, name)
								fmtProgress[idx] = len(s.alert.Rule.Formatters())
							} else {
								s.deadLetter = true
							}
						} else {
							formattersApplied.WithLabelValues(name).Inc()
							fmtProgress[idx]++
						}
					}
				}
			}(name, idxs)
		}
		wg.Wait()
	}

	// Write results.
	for _, s := range states {
		if s.deadLetter && service.dlq != nil {
			payload, err := alerts.Marshal(s.alert)
			if err != nil {
				writeErrors.Inc()
				service.Error(errors.NewE(err))
				continue
			}
			if err := service.dlq.WriteMessages(ctx, broker.Message{Key: s.key, Value: payload}); err != nil {
				writeErrors.Inc()
				service.Error(errors.NewE(err))
			} else {
				alertsDLQ.Inc()
			}
			continue
		}

		payload, merr := alerts.Marshal(s.alert)
		if merr != nil {
			writeErrors.Inc()
			service.Error(errors.NewE(merr))
			continue
		}
		if err := service.writer.WriteMessages(ctx, broker.Message{Key: s.key, Value: payload}); err != nil {
			writeErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		alertsOut.Inc()
	}
}

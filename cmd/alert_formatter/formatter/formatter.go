package formatter

import (
	"context"

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

// Reads alerts from Kafka, applies formatters, and writes to the dispatcher topic.
func (service *FormatterService) Run(ctx context.Context) errors.Error {
	return services.RunAlertPipeline(ctx, service.Logger, service.reader, service.writer, service.dlq, 50, 4,
		services.PipelineCounters{
			In: alertsIn.Inc, Out: alertsOut.Inc, DLQ: alertsDLQ.Inc,
			ParseError: parseErrors.Inc, WriteError: writeErrors.Inc,
		},
		func(ctx context.Context, _ []byte, alert *alerts.Alert) (skip bool, deadLetter bool) {
			service.Info("applying formatters for alert %s", alert.AlertID)

			snapshot, merr := alerts.Marshal(alert)
			if merr != nil {
				service.Error(errors.NewE(merr))
				return false, false
			}

			for _, name := range alert.Rule.Formatters() {
				_, absent, removed, err := service.pool.Format(ctx, name, alert, "")
				switch {
				case removed || absent:
					label := "not found"
					if removed {
						label = "removed"
					}
					service.Error(errors.NewF("formatter %s %s - alert %s missing formatter", name, label, alert.AlertID))
					alert.Attempts++
					if alert.Attempts >= services.MaxPluginAttempts {
						service.Info("alert %s passed through after %d attempts (formatter unavailable)", alert.AlertID, alert.Attempts)
						continue
					}
					return false, true
				case err != nil:
					formatterErrors.WithLabelValues(name).Inc()
					service.Error(err)
					if restored, uerr := alerts.Unmarshal(snapshot); uerr == nil {
						*alert = *restored
					}
					return false, false
				default:
					formattersApplied.WithLabelValues(name).Inc()
				}
			}
			return false, false
		},
	)
}

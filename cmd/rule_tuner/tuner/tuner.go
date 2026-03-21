package tuner

import (
	"context"
	stderrors "errors"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	pools "github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
	"github.com/harishhary/blink/pkg/tuning_rules"
	tuningcatalog "github.com/harishhary/blink/pkg/tuning_rules/pool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	alertsIn          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_in_total"})
	alertsOut         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_out_total"})
	alertsDLQ         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_dlq_total"})
	alertsIgnored     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_ignored_total"})
	confidenceChanged = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "confidence_changed_total"})
	tuningErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "errors_total"})
	parseErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "parse_errors_total"})
	writeErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "write_errors_total"})
)

// tuneResult holds the outcome of a single tuning rule evaluation.
type tuneResult struct {
	ruleType   tuning_rules.RuleType
	confidence scoring.Confidence
	applies    bool
}

// TunerService reads alerts from Kafka, applies tuning rules, and writes to the enricher topic.
type TunerService struct {
	svcctx.ServiceContext
	reader broker.Reader
	writer broker.Writer
	dlq    broker.Writer
	pool   *tuningcatalog.Pool
}

func NewTunerService(pool *tuningcatalog.Pool) (*TunerService, error) {
	serviceContext := svcctx.New("BLINK-RULE-TUNER - TUNER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	cfg := serviceContext.Configuration()
	b := kafka.NewKafkaBroker(cfg.Kafka)
	reader := b.NewReader(cfg.Topics.TunerTopic, cfg.Topics.TunerGroup)
	writer := b.NewWriter(cfg.Topics.EnricherTopic)

	var dlq broker.Writer
	if cfg.Topics.TunerDLQTopic != "" {
		dlq = b.NewWriter(cfg.Topics.TunerDLQTopic)
	}

	return &TunerService{
		ServiceContext: serviceContext,
		reader:         reader,
		writer:         writer,
		dlq:            dlq,
		pool:           pool,
	}, nil
}

func (service *TunerService) Name() string { return "rule-tuner" }

func (service *TunerService) Run(ctx context.Context) errors.Error {
	return services.RunAlertPipeline(ctx, service.Logger, service.reader, service.writer, service.dlq, 50,
		services.PipelineCounters{
			In: alertsIn.Inc, Out: alertsOut.Inc, DLQ: alertsDLQ.Inc,
			ParseError: parseErrors.Inc, WriteError: writeErrors.Inc,
		},
		func(ctx context.Context, _ []byte, alert *alerts.Alert) (skip bool, deadLetter bool) {
			service.Info("applying tuning rules for alert %s", alert.AlertID)

			var results []tuneResult
			for _, name := range alert.Rule.TuningRules() {
				var res tuneResult
				if err := service.pool.Call(ctx, name, "", func(callCtx context.Context, r tuning_rules.TuningRule) error {
					if !r.Enabled() {
						return nil
					}
					res.ruleType = r.RuleType()
					res.confidence = r.Confidence()
					applies, e := r.Tune(callCtx, *alert)
					if e != nil {
						return e
					}
					res.applies = applies
					return nil
				}); err != nil {
					if stderrors.Is(err, pools.ErrPluginRemoved) || stderrors.Is(err, pools.ErrPluginNotFound) {
						label := "not found"
						if stderrors.Is(err, pools.ErrPluginRemoved) {
							label = "removed"
						}
						service.Error(errors.NewF("tuning rule %s %s - alert %s missing tuning", name, label, alert.AlertID))
						alert.Attempts++
						if alert.Attempts >= services.MaxPluginAttempts {
							service.Info("alert %s passed through after %d attempts (tuning rule unavailable)", alert.AlertID, alert.Attempts)
							continue
						}
						return false, true
					}
					service.Error(errors.NewE(err))
					tuningErrors.Inc()
					return false, false
				}
				if res.applies {
					results = append(results, res)
				}
			}

			before := alert.Confidence
			confidence, ignored := applyTuningResults(alert.Confidence, results)
			if ignored {
				service.Info("alert %s ignored by tuning rule", alert.AlertID)
				alertsIgnored.Inc()
				return true, false
			}
			if confidence != before {
				confidenceChanged.Inc()
			}
			alert.Confidence = confidence
			return false, false
		},
	)
}

// applyTuningResults applies tuning results in priority order: Ignore > SetConfidence > Increase/Decrease.
// Returns (confidence, ignored). When ignored=true the alert should be discarded.
func applyTuningResults(base scoring.Confidence, results []tuneResult) (scoring.Confidence, bool) {
	confidence := base

	// Process Ignore rules first.
	for _, res := range results {
		if res.ruleType == tuning_rules.Ignore {
			return 0, true
		}
	}

	// Process SetConfidence rules next; highest confidence wins.
	setByRule := false
	for _, res := range results {
		if res.ruleType == tuning_rules.SetConfidence {
			if !setByRule || res.confidence > confidence {
				confidence = res.confidence
				setByRule = true
			}
		}
	}

	if setByRule {
		return confidence, false
	}

	// Process Increase/Decrease confidence rules last.
	for _, res := range results {
		if res.ruleType == tuning_rules.IncreaseConfidence && res.confidence > confidence {
			confidence = res.confidence
		} else if res.ruleType == tuning_rules.DecreaseConfidence && res.confidence < confidence {
			confidence = res.confidence
		}
	}

	return confidence, false
}

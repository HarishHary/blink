package tuner

import (
	"context"
	stderrors "errors"
	"sync"

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

// tuneResult holds the outcome of a single tuning rule evaluation for one alert.
type tuneResult struct {
	ruleType   tuning_rules.RuleType
	confidence scoring.Confidence
	applies    bool
}

// alertState groups a decoded alert with its accumulated tuning results.
type alertState struct {
	key        []byte
	alert      *alerts.Alert
	results    []tuneResult
	deadLetter bool
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

func (service *TunerService) processBatch(ctx context.Context, msgs []broker.Message) {
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

	// Group by tuning rule name: name => indices into states.
	byRule := make(map[string][]int)
	for i, s := range states {
		for _, name := range s.alert.Rule.TuningRules() {
			byRule[name] = append(byRule[name], i)
		}
	}

	// Fan out: one goroutine per tuning rule with all its alerts.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, idxs := range byRule {
		wg.Add(1)
		go func(name string, idxs []int) {
			defer wg.Done()

			copies := make([]alerts.Alert, len(idxs))
			for j, idx := range idxs {
				copies[j] = *states[idx].alert
			}

			ruleType, confidence, applies, err := service.pool.Tune(ctx, name, copies, "")
			if err != nil {
				if stderrors.Is(err, pools.ErrPluginRemoved) || stderrors.Is(err, pools.ErrPluginNotFound) {
					label := "not found"
					if stderrors.Is(err, pools.ErrPluginRemoved) {
						label = "removed"
					}
					service.Error(errors.NewF("tuning rule %s %s", name, label))
					mu.Lock()
					for _, idx := range idxs {
						states[idx].deadLetter = true
					}
					mu.Unlock()
					return
				}
				service.Error(errors.NewE(err))
				tuningErrors.Inc()
				return
			}

			mu.Lock()
			for j, idx := range idxs {
				if applies[j] {
					states[idx].results = append(states[idx].results, tuneResult{
						ruleType: ruleType, confidence: confidence, applies: true,
					})
				}
			}
			mu.Unlock()
		}(name, idxs)
	}
	wg.Wait()

	// Apply results and write.
	for _, s := range states {
		if s.deadLetter {
			s.alert.Attempts++
			if s.alert.Attempts >= services.MaxPluginAttempts || service.dlq == nil {
				service.Info("alert %s passed through after %d attempts (tuning rule unavailable)", s.alert.AlertID, s.alert.Attempts)
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
					service.Error(errors.NewE(err))
				} else {
					alertsDLQ.Inc()
				}
				continue
			}
		}

		before := s.alert.Confidence
		confidence, ignored := applyTuningResults(s.alert.Confidence, s.results)
		if ignored {
			service.Info("alert %s ignored by tuning rule", s.alert.AlertID)
			alertsIgnored.Inc()
			continue
		}
		if confidence != before {
			confidenceChanged.Inc()
		}
		s.alert.Confidence = confidence

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

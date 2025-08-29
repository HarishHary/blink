package executor

import (
	stdctx "context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	ctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
)

var (
	batchSizeHist  = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_size"})
	eventsIn       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_in_total"})
	alertsOut      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_out_total"})                                  // Total alerts emitted (existing metric)
	ruleEvalHist   = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_evaluation_seconds"}, []string{"rule"})  // Per‑rule evaluation latency
	ruleEvalErrors = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_evaluation_errors_total"}, []string{"rule"}) // Per‑rule evaluation errors

	readBatchErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_errors_total"}) // Errors returned by ReadBatch()
	readBatchDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_seconds"})  // Duration of each ReadBatch() call
	commitErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_errors_total"})     // Errors returned by CommitMessages()
	commitDuration    = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_seconds"})      // Duration of each CommitMessages() call

	eventsParseErrors    = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_parse_errors_total"})     // Number of events failing JSON unmarshal
	eventsInvalidLogType = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_invalid_log_type_total"}) // Events missing or having a non‑string log_type
	eventsNoRules        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_no_rules_total"})         // Events whose log_type has no matching rules
	batchProcessDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_processing_seconds"})  // Total time to process a full batch (read→evaluate→write→commit)
	concurrencyGauge     = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "concurrent_events"})                 // Current in‑flight (concurrent) event evaluations

	alertsWriteErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_errors_total"}) // Duration of writing alerts to Kafka
	alertsWriteDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_seconds"})  // Errors encountered writing alerts

	ruleMatches   = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_matches_total"}, []string{"rule"}) // Per‑rule count of successful rule matches
	rulesPerEvent = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rules_per_event"})                     // Histogram of how many rules are evaluated per event
)

// ExecutorService reads batches of events, applies rules, and writes alerts.
type ExecutorService struct {
	ctx        ctx.ServiceContext
	reader     broker.Reader
	writer     broker.Writer
	ruleRepo   *rules.RuleRepository
	sem        *semaphore.Weighted
	batchSize  int
	timeoutSec int
}

// New constructs a rule executor service using Kafka topics.
func New() *ExecutorService {
	serviceContext := ctx.New("BLINK-RULE-EXECUTOR - EXEC")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	broker := kafka.NewKafkaBroker(serviceContext.Configuration().Kafka)
	reader := broker.NewReader(
		serviceContext.Configuration().Topics.ExecTopic,
		serviceContext.Configuration().Topics.ExecGroup,
	)
	writer := broker.NewWriter(serviceContext.Configuration().Topics.TunerTopic)

	// Read executor settings from config
	ecfg := serviceContext.Configuration().Executor
	bs := ecfg.BatchSize
	if bs <= 0 {
		bs = 50
	}
	conc := ecfg.Concurrency
	if conc <= 0 {
		conc = 4
	}
	to := ecfg.TimeoutSec
	if to <= 0 {
		to = 10
	}

	return &ExecutorService{
		ctx:        serviceContext,
		reader:     reader,
		writer:     writer,
		ruleRepo:   rules.GetRuleRepository(),
		sem:        semaphore.NewWeighted(int64(conc)),
		batchSize:  bs,
		timeoutSec: to,
	}
}

// Name returns the executor service name.
func (service *ExecutorService) Name() string { return "rule-executor" }

// Run reads batches, applies rules concurrently with timeout, then commits offsets.
func (service *ExecutorService) Run() errors.Error {
	ctx := stdctx.Background()
	for {
		batchStart := time.Now()

		msgs, err := service.reader.ReadBatch(ctx, service.batchSize)
		readBatchDuration.Observe(time.Since(batchStart).Seconds())
		if err != nil {
			readBatchErrors.Inc()
			service.ctx.Error(errors.NewE(err))
			continue
		}
		batchSizeHist.Observe(float64(len(msgs)))
		eventsIn.Add(float64(len(msgs)))

		var wg sync.WaitGroup
		for _, m := range msgs {
			wg.Add(1)
			go func(m broker.Message) {
				defer wg.Done()
				if err := service.sem.Acquire(ctx, 1); err != nil {
					service.ctx.Error(errors.NewE(err))
					return
				}
				concurrencyGauge.Inc()
				defer func() {
					service.sem.Release(1)
					concurrencyGauge.Dec()
				}()

				// per-event evaluation timeout
				cctx, cancel := stdctx.WithTimeout(ctx, time.Duration(service.timeoutSec)*time.Second)
				defer cancel()
				service.processOne(cctx, m)
			}(m)
		}
		wg.Wait()
		startCommit := time.Now()
		if err := service.reader.CommitMessages(ctx, msgs...); err != nil {
			commitErrors.Inc()
			service.ctx.Error(errors.NewE(err))
		}
		commitDuration.Observe(time.Since(startCommit).Seconds())
		batchProcessDuration.Observe(time.Since(batchStart).Seconds())
	}
}

// processOne evaluates rules on one event and writes alerts if matched.
func (service *ExecutorService) processOne(ctx stdctx.Context, m broker.Message) {
	var event map[string]any
	if err := json.Unmarshal(m.Value, &event); err != nil {
		eventsParseErrors.Inc()
		service.ctx.Error(errors.NewE(err))
		return
	}

	lt, ok := event["log_type"].(string)
	if !ok {
		eventsInvalidLogType.Inc()
		return
	}

	rules := service.ruleRepo.GetRulesForLogType(lt)
	rulesPerEvent.Observe(float64(len(rules)))
	if len(rules) == 0 {
		eventsNoRules.Inc()
		return
	}

	service.ctx.Info("evaluating rules for log_type %s", lt)
	for _, rule := range rules {
		if !rule.Enabled() || !rule.SubKeysInEvent(event) {
			continue
		}

		startEval := time.Now()
		passed, err := rule.Evaluate(event)
		ruleEvalHist.WithLabelValues(rule.Name()).Observe(time.Since(startEval).Seconds())
		if err != nil {
			ruleEvalErrors.WithLabelValues(rule.Name()).Inc()
			service.ctx.Error(err)
			continue
		}
		if !passed {
			continue
		}

		ruleMatches.WithLabelValues(rule.Name()).Inc()
		alertsOut.Inc()

		alert, err := alerts.NewAlert(rule, event)
		if err != nil {
			service.ctx.Error(err)
			continue
		}

		payload, _ := json.Marshal(alert)
		startWrite := time.Now()
		if err := service.writer.WriteMessages(ctx, broker.Message{Key: m.Key, Value: payload}); err != nil {
			alertsWriteErrors.Inc()
			service.ctx.Error(errors.NewE(err))
		} else {
			alertsWriteDuration.Observe(time.Since(startWrite).Seconds())
		}
	}
}

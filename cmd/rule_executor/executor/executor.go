package executor

import (
	"context"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	ctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	execpb "github.com/harishhary/blink/internal/exec/pb"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/rules/config"
	rulecatalog "github.com/harishhary/blink/pkg/rules/pool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
	proto "google.golang.org/protobuf/proto"
)

var (
	batchSizeHist  = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_size"})
	eventsIn       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_in_total"})
	alertsOut      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_out_total"})
	ruleEvalHist   = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_evaluation_seconds"}, []string{"rule"})
	ruleEvalErrors = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_evaluation_errors_total"}, []string{"rule"})

	readBatchErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_errors_total"})
	readBatchDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_seconds"})
	commitErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_errors_total"})
	commitDuration    = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_seconds"})

	eventsParseErrors    = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_parse_errors_total"})
	eventsInvalidLogType = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_invalid_log_type_total"})
	eventsNoRules        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_no_rules_total"})
	batchProcessDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_processing_seconds"})
	concurrencyGauge     = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "concurrent_events"})

	alertsWriteErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_errors_total"})
	alertsWriteDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_seconds"})

	ruleMatches   = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_matches_total"}, []string{"rule"})
	rulesPerEvent = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rules_per_event"})
)

// Reads ExecMessages from blink-exec, applies the routed rules, and writes alerts to blink-merger.
type ExecutorService struct {
	ctx.ServiceContext
	reader     broker.Reader
	writer     broker.Writer
	pool       *rulecatalog.Pool
	cfgWatcher *config.Watcher
	sem        *semaphore.Weighted
	batchSize  int
	timeoutSec int
}

func NewExecutorService(pool *rulecatalog.Pool, cfgWatcher *config.Watcher) (*ExecutorService, error) {
	serviceContext := ctx.New("BLINK-RULE-EXECUTOR - EXEC")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	b := kafka.NewKafkaBroker(serviceContext.Configuration().Kafka)
	reader := b.NewReader(
		serviceContext.Configuration().Topics.ExecTopic,
		serviceContext.Configuration().Topics.ExecGroup,
	)
	writer := b.NewWriter(serviceContext.Configuration().Topics.MergerTopic)

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
		ServiceContext: serviceContext,
		reader:         reader,
		writer:         writer,
		pool:           pool,
		cfgWatcher:     cfgWatcher,
		sem:            semaphore.NewWeighted(int64(conc)),
		batchSize:      bs,
		timeoutSec:     to,
	}, nil
}

func (service *ExecutorService) Name() string { return "rule-executor" }

func (service *ExecutorService) Run(ctx context.Context) errors.Error {
	for {
		batchStart := time.Now()

		msgs, err := service.reader.ReadBatch(ctx, service.batchSize)
		readBatchDuration.Observe(time.Since(batchStart).Seconds())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			readBatchErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		batchSizeHist.Observe(float64(len(msgs)))
		eventsIn.Add(float64(len(msgs)))

		// Snapshot the registry once per batch so all concurrent goroutines
		// evaluate against the same generation of rule config.
		snapshot := service.cfgWatcher.Current()

		var wg sync.WaitGroup
		for _, m := range msgs {
			wg.Add(1)
			go func(m broker.Message) {
				defer wg.Done()
				if err := service.sem.Acquire(ctx, 1); err != nil {
					return // ctx cancelled
				}
				concurrencyGauge.Inc()
				defer func() {
					service.sem.Release(1)
					concurrencyGauge.Dec()
				}()

				cctx, cancel := context.WithTimeout(ctx, time.Duration(service.timeoutSec)*time.Second)
				defer cancel()
				service.processOne(cctx, m, snapshot)
			}(m)
		}
		wg.Wait()

		startCommit := time.Now()
		if err := service.reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			commitErrors.Inc()
			service.Error(errors.NewE(err))
		}
		commitDuration.Observe(time.Since(startCommit).Seconds())
		batchProcessDuration.Observe(time.Since(batchStart).Seconds())
	}
}

// processOne decodes an ExecMessage, filters rules to the routed subset, and evaluates each eligible rule against the event.
func (service *ExecutorService) processOne(ctx context.Context, m broker.Message, snapshot *config.Registry) {
	var msg execpb.ExecMessage
	if err := proto.Unmarshal(m.Value, &msg); err != nil {
		eventsParseErrors.Inc()
		service.Error(errors.NewE(err))
		return
	}

	event := msg.GetEvent().AsMap()

	lt, ok := event["log_type"].(string)
	if !ok {
		eventsInvalidLogType.Inc()
		return
	}

	metaList := service.eligibleRules(snapshot, lt, msg.GetRuleIds())
	rulesPerEvent.Observe(float64(len(metaList)))
	if len(metaList) == 0 {
		eventsNoRules.Inc()
		return
	}

	tenantID, _ := event["tenant_id"].(string)

	service.Info("evaluating %d rule(s) for log_type=%s", len(metaList), lt)
	for _, meta := range metaList {
		if !meta.Enabled() {
			continue
		}
		if len(meta.ReqSubkeys()) > 0 && !rules.DefaultSubKeysInEvent(meta, event) {
			continue
		}

		startEval := time.Now()
		passed, err := service.pool.Evaluate(ctx, meta.Id(), event, tenantID)
		ruleEvalHist.WithLabelValues(meta.Name()).Observe(time.Since(startEval).Seconds())
		if err != nil {
			ruleEvalErrors.WithLabelValues(meta.Name()).Inc()
			service.Error(err)
			continue
		}
		if !passed {
			continue
		}

		ruleMatches.WithLabelValues(meta.Name()).Inc()
		alertsOut.Inc()

		alert, err := alerts.NewAlert(meta, event)
		if err != nil {
			service.Error(err)
			continue
		}

		payload, _ := alerts.Marshal(alert)
		startWrite := time.Now()
		if err := service.writer.WriteMessages(ctx, broker.Message{Key: m.Key, Value: payload}); err != nil {
			alertsWriteErrors.Inc()
			service.Error(errors.NewE(err))
		} else {
			alertsWriteDuration.Observe(time.Since(startWrite).Seconds())
		}
	}
}

// eligibleRules returns the rule metadata to evaluate for this event.
func (service *ExecutorService) eligibleRules(snapshot *config.Registry, logType string, ruleIDs []string) []*config.RuleMetadata {
	all := snapshot.RulesForLogType(logType)
	if len(ruleIDs) == 0 {
		return all
	}

	idSet := make(map[string]struct{}, len(ruleIDs))
	for _, id := range ruleIDs {
		idSet[id] = struct{}{}
	}

	var result []*config.RuleMetadata
	for _, meta := range all {
		if _, ok := idSet[meta.Id()]; ok {
			result = append(result, meta)
		}
	}
	return result
}

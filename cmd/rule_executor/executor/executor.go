package executor

import (
	"context"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/brokers/kafka"
	"github.com/harishhary/blink/internal/configuration"
	ctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	execpb "github.com/harishhary/blink/internal/exec/pb"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/scoring"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
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
	rulesPerBatch        = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rules_per_batch"})
	concurrencyGauge     = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "concurrent_rules"})

	alertsWriteErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_errors_total"})
	alertsWriteDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_seconds"})

	ruleMatches = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_matches_total"}, []string{"rule"})
)

// ruleEntry groups the events (and the first event's tenantID for canary routing)
// that should be evaluated against a single rule within one Kafka batch.
type ruleEntry struct {
	meta     *rules.RuleMetadata
	events   []events.Event // subset of batch events eligible for this rule
	tenantID string         // used as canary routing key; taken from first event
}

// Reads ExecMessages from blink-exec, applies the routed rules, and writes alerts to blink-merger.
type ExecutorService struct {
	ctx.ServiceContext
	reader     brokers.Reader
	writer     brokers.Writer
	pool       *rules.Pool
	cfgWatcher *rules.RuleConfigManager
	sem        *semaphore.Weighted
	batchSize  int
	timeoutSec int
}

func NewExecutorService(pool *rules.Pool, cfgWatcher *rules.RuleConfigManager) (*ExecutorService, error) {
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

		service.processBatch(ctx, msgs, snapshot)

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

// processBatch decodes all messages, groups events by eligible rule, then fans
// out one Evaluate call per rule with bounded concurrency.
//
// gRPC calls = len(distinct eligible rules), regardless of batch size.
// Previously: gRPC calls = len(msgs) × len(rules per event).
func (service *ExecutorService) processBatch(ctx context.Context, msgs []brokers.Message, snapshot *rules.RuleRegistry) {
	// Step 1: decode messages and index events by rule.
	byRule := make(map[string]*ruleEntry)
	decodedAny := false

	for _, m := range msgs {
		var msg execpb.ExecMessage
		if err := proto.Unmarshal(m.Value, &msg); err != nil {
			eventsParseErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}

		event := msg.GetEvent().AsMap()
		lt, ok := event["log_type"].(string)
		if !ok {
			eventsInvalidLogType.Inc()
			continue
		}

		metaList := service.eligibleRules(snapshot, lt, msg.GetRuleIds())
		if len(metaList) == 0 {
			eventsNoRules.Inc()
			continue
		}
		decodedAny = true

		tenantID, _ := event["tenant_id"].(string)

		for _, meta := range metaList {
			if !meta.Enabled {
				continue
			}
			if len(meta.ReqSubkeys()) > 0 && !rules.DefaultSubKeysInEvent(meta, event) {
				continue
			}
			e, exists := byRule[meta.Id]
			if !exists {
				e = &ruleEntry{meta: meta, tenantID: tenantID}
				byRule[meta.Id] = e
			}
			e.events = append(e.events, event)
		}
	}

	if !decodedAny || len(byRule) == 0 {
		return
	}

	rulesPerBatch.Observe(float64(len(byRule)))
	service.Info("evaluating %d rule(s) across batch of %d message(s)", len(byRule), len(msgs))

	// Step 2: fan out - one goroutine per rule, bounded by the semaphore.
	var wg sync.WaitGroup
	for _, entry := range byRule {
		wg.Add(1)
		go func(entry *ruleEntry) {
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
			service.evaluateRule(cctx, entry)
		}(entry)
	}
	wg.Wait()
}

// evaluateRule calls Evaluate for one rule against all its candidate events,
// then writes an alert to blink-merger for each event that matched.
func (service *ExecutorService) evaluateRule(ctx context.Context, entry *ruleEntry) {
	startEval := time.Now()
	results, err := service.pool.Evaluate(ctx, entry.meta.Id, entry.events, entry.tenantID)
	ruleEvalHist.WithLabelValues(entry.meta.Name).Observe(time.Since(startEval).Seconds())
	if err != nil {
		ruleEvalErrors.WithLabelValues(entry.meta.Name).Inc()
		service.Error(err)
		return
	}

	for i, result := range results {
		if !result.Matched {
			continue
		}
		ruleMatches.WithLabelValues(entry.meta.Name).Inc()
		alertsOut.Inc()

		alert, err := alerts.NewAlert(entry.meta, entry.events[i])
		if err != nil {
			service.Error(err)
			continue
		}

		// Apply optional per-event overrides from the plugin.
		if len(result.MergeByKeys) > 0 {
			alert.OverrideMergeByKeys = result.MergeByKeys
		}
		if result.Severity != "" {
			if sev, err := scoring.ParseSeverity(result.Severity); err == nil {
				alert.Severity = sev
			} else {
				service.Error(errors.NewF("rule %s returned invalid severity %q: %v", entry.meta.Name, result.Severity, err))
			}
		}
		for k, v := range result.Context {
			alert.Event[k] = v
		}

		payload, _ := alerts.Marshal(alert)
		startWrite := time.Now()
		if err := service.writer.WriteMessages(ctx, brokers.Message{
			Key:   []byte(alert.MergePartitionKey()),
			Value: payload,
		}); err != nil {
			alertsWriteErrors.Inc()
			service.Error(errors.NewE(err))
		} else {
			alertsWriteDuration.Observe(time.Since(startWrite).Seconds())
		}
	}
}

// eligibleRules returns the rule metadata to evaluate for this event.
func (service *ExecutorService) eligibleRules(snapshot *rules.RuleRegistry, logType string, ruleIDs []string) []*rules.RuleMetadata {
	all := rules.RulesForLogType(snapshot, logType)
	if len(ruleIDs) == 0 {
		return all
	}

	idSet := make(map[string]struct{}, len(ruleIDs))
	for _, id := range ruleIDs {
		idSet[id] = struct{}{}
	}

	var result []*rules.RuleMetadata
	for _, meta := range all {
		if _, ok := idSet[meta.Id]; ok {
			result = append(result, meta)
		}
	}
	return result
}

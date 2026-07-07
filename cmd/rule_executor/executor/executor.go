package executor

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/config"
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

// ruleEntry groups the events (and the first event's tenantID for canary rollout)
// that should be evaluated against a single rule within one Kafka batch.
type ruleEntry struct {
	meta     *rules.RuleMetadata
	events   []events.Event // subset of batch events eligible for this rule
	tenantID string         // used as canary rollout key; taken from first event
}

// Reads ExecMessages from blink-exec, applies the rolled out rules, and writes alerts to blink-merger.
type ExecutorService struct {
	*logger.Logger
	reader     brokers.Reader
	writer     brokers.Writer
	pool       *rules.Pool
	cfg        config.Source[*rules.RuleMetadata]
	sem        *semaphore.Weighted
	batchSize  int
	timeoutSec int
}

// Config is the explicit set of dependencies NewExecutorService needs. The composition
// root (cmd/rule_executor/main) loads config once and injects these, rather than the
// component reaching into a shared ServiceContext / re-loading the whole environment.
// Config's topic fields (and the embedded Tuning knobs) are populated from the environment
// by main (which embeds it); Broker is injected after load.
type Config struct {
	Broker      brokers.Broker
	ExecTopic   string `env:"KAFKA_TOPIC_EXEC"`
	ExecGroup   string `env:"KAFKA_GROUP_EXEC"`
	MergerTopic string `env:"KAFKA_TOPIC_MERGER"`
	Tuning      Tuning
}

// Tuning holds the executor's performance knobs, loaded from EXECUTOR_* env vars.
type Tuning struct {
	// BatchSize is the maximum number of events to read in one batch.
	BatchSize int `env:"EXECUTOR_BATCH_SIZE,optional"`
	// Concurrency is the max number of parallel rule evaluations.
	Concurrency int `env:"EXECUTOR_CONCURRENCY,optional"`
	// TimeoutSec is the per-event evaluation timeout in seconds.
	TimeoutSec int `env:"EXECUTOR_TIMEOUT_SEC,optional"`
}

func NewExecutorService(c Config, pool *rules.Pool, cfg config.Source[*rules.RuleMetadata]) *ExecutorService {
	bs := c.Tuning.BatchSize
	if bs <= 0 {
		bs = 50
	}
	conc := c.Tuning.Concurrency
	if conc <= 0 {
		conc = 4
	}
	to := c.Tuning.TimeoutSec
	if to <= 0 {
		to = 10
	}

	return &ExecutorService{
		Logger:     logger.New("rule-executor", "dev"),
		reader:     c.Broker.NewReader(c.ExecTopic, c.ExecGroup),
		writer:     c.Broker.NewWriter(c.MergerTopic),
		pool:       pool,
		cfg:        cfg,
		sem:        semaphore.NewWeighted(int64(conc)),
		batchSize:  bs,
		timeoutSec: to,
	}
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

		// Resolve the rule set once per batch so all concurrent goroutines evaluate
		// against the same generation of control-plane config (cached by generation).
		ruleSet := service.cfg.Primaries()

		service.processBatch(ctx, msgs, ruleSet)

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
func (service *ExecutorService) processBatch(ctx context.Context, msgs []brokers.Message, ruleSet []*rules.RuleMetadata) {
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

		metaList := service.eligibleRules(ruleSet, lt, msg.GetRuleIds())
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
			if len(meta.ReqSubkeys) > 0 && !rules.DefaultSubKeysInEvent(meta, event) {
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

		alert, err := alerts.NewAlert(entry.meta, mergeEventContext(entry.events[i], result.Context))
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
func (service *ExecutorService) eligibleRules(ruleSet []*rules.RuleMetadata, logType string, ruleIDs []string) []*rules.RuleMetadata {
	all := rules.RulesForLogTypeIn(ruleSet, logType)
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

// mergeEventContext returns a fresh event carrying the original event's fields plus the plugin's
// context, WITHOUT mutating the original. entry.events[i] is shared across every rule that matched
// the event in this batch (and is read concurrently by the shadow copy), so writing into it would
// race; a new map keeps it immutable. Returns the original unchanged when ctx is empty (no alloc).
func mergeEventContext(event events.Event, ctx map[string]any) events.Event {
	if len(ctx) == 0 {
		return event
	}
	merged := make(events.Event, len(event)+len(ctx))
	maps.Copy(merged, event)
	maps.Copy(merged, ctx)
	return merged
}

package executor

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/brokers"
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

type ruleEntry struct {
	meta    *rules.RuleMetadata
	events  []events.Event
	results []rules.EvaluateItem
}

// Reads ExecMessages from blink-exec, applies the rolled out rules, and writes alerts to blink-merger.
type Service struct {
	logger       *logger.Logger
	config       Config
	mergerWriter brokers.Writer
	dlqWriter    brokers.Writer
	pool         *rules.Pool
	ruleCfg      *rules.SnapshotConfig
	sem          *semaphore.Weighted
}

// Config is the explicit set of dependencies NewExecutorService needs. The composition
// root (cmd/rule_executor/main) loads config once and injects these, rather than the
// component reaching into a shared ServiceContext / re-loading the whole environment.
// Config's topic fields (and the embedded Tuning knobs) are populated from the environment
// by main (which embeds it); Broker is injected after load.
type Config struct {
	Broker        brokers.Broker
	ExecutorTopic string `env:"KAFKA_TOPIC_EXECUTOR"`
	ExecutorGroup string `env:"KAFKA_GROUP_EXECUTOR"`
	MergerTopic   string `env:"KAFKA_TOPIC_MERGER"`
	DLQTopic      string `env:"KAFKA_TOPIC_EXECUTOR_DLQ"`
	// Ready gates grouped-consumer creation until the rule snapshot catch-up completes.
	Ready func() bool
	// BatchSize is the number of messages to read from the broker at once.
	BatchSize int `env:"EXECUTOR_BATCH_SIZE,optional"`
	// Concurrency is the maximum number of parallel executor plugin calls.
	Concurrency int `env:"EXECUTOR_CONCURRENCY,optional"`
	// TimeoutSec bounds each executor pool call in seconds.
	TimeoutSec int `env:"EXECUTOR_TIMEOUT_SEC,optional"`
	// MaxAttempts is how many times a failing executor call is tried per event before DLQ.
	MaxAttempts int `env:"EXECUTOR_MAX_ATTEMPTS,optional"`
	// RetryBaseMS is the initial executor and publication retry delay in milliseconds.
	RetryBaseMS int `env:"EXECUTOR_RETRY_BASE_MS,optional"`
	// RetryCapMS bounds exponential executor and publication retry delays in milliseconds.
	RetryCapMS int `env:"EXECUTOR_RETRY_CAP_MS,optional"`
}

func NewExecutorService(logger *logger.Logger, cfg Config, pool *rules.Pool, ruleCfg *rules.SnapshotConfig) *Service {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 10
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.RetryBaseMS <= 0 {
		cfg.RetryBaseMS = 100
	}
	if cfg.RetryCapMS <= 0 {
		cfg.RetryCapMS = 5000
	}
	if cfg.RetryCapMS < cfg.RetryBaseMS {
		cfg.RetryCapMS = cfg.RetryBaseMS
	}

	return &Service{
		logger:       logger,
		config:       cfg,
		mergerWriter: cfg.Broker.NewWriter(cfg.MergerTopic),
		dlqWriter:    cfg.Broker.NewWriter(cfg.DLQTopic),
		pool:         pool,
		ruleCfg:      ruleCfg,
		sem:          semaphore.NewWeighted(int64(cfg.Concurrency)),
	}
}

func (s *Service) Name() string { return "rule-executor" }

func (s *Service) Run(ctx context.Context) (runErr errors.Error) {
	if !waitForReady(ctx, s.config.Ready) {
		return nil
	}

	s.logger.Info("catalogs ready; consuming events (topic=%s group=%s)", s.config.ExecutorTopic, s.config.ExecutorGroup)
	reader := s.config.Broker.NewReader(s.config.ExecutorTopic, s.config.ExecutorGroup)
	defer func() {
		if err := reader.Close(); err != nil && runErr == nil && ctx.Err() == nil {
			runErr = errors.NewE(err)
		}
	}()

	for {
		batchStart := time.Now()

		msgs, err := reader.ReadBatch(ctx, s.config.BatchSize)
		readBatchDuration.Observe(time.Since(batchStart).Seconds())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			readBatchErrors.Inc()
			return errors.NewE(err)
		}
		batchSizeHist.Observe(float64(len(msgs)))
		eventsIn.Add(float64(len(msgs)))

		// Resolve the rule set once per batch so all concurrent goroutines evaluate
		// against the same generation of control-plane config (cached by generation).
		ruleSet := s.ruleCfg.Primaries()

		if err := s.processBatch(ctx, msgs, ruleSet); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		startCommit := time.Now()
		if err := reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			commitErrors.Inc()
			return errors.NewE(err)
		}
		commitDuration.Observe(time.Since(startCommit).Seconds())
		batchProcessDuration.Observe(time.Since(batchStart).Seconds())
	}
}

// waitForReady polls the snapshot readiness callback without creating the grouped reader.
// The callback is optional for backward-compatible construction in focused tests.
func waitForReady(ctx context.Context, ready func() bool) bool {
	if ready == nil {
		return true
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return ctx.Err() == nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// processBatch decodes messages, groups events by rule and rollout bucket, evaluates
// every cohort, then publishes each rule's matched events in original input order.
func (s *Service) processBatch(ctx context.Context, msgs []brokers.Message, ruleSet []*rules.RuleMetadata) errors.Error {
	byRule := make(map[string]*ruleEntry)
	ruleOrder := make([]*ruleEntry, 0)

	for _, m := range msgs {
		var msg execpb.ExecMessage
		if err := proto.Unmarshal(m.Value, &msg); err != nil {
			eventsParseErrors.Inc()
			return errors.NewE(err)
		}

		if msg.GetEvent() == nil {
			eventsParseErrors.Inc()
			return errors.NewF("exec message has no event")
		}
		event := msg.GetEvent().AsMap()
		lt, ok := event["log_type"].(string)
		if !ok {
			eventsInvalidLogType.Inc()
			return errors.NewF("event log_type must be a string")
		}

		metaList, err := s.eligibleRules(ruleSet, lt, msg.GetRuleIds())
		if err != nil {
			return err
		}
		if len(metaList) == 0 {
			eventsNoRules.Inc()
			continue
		}

		for _, meta := range metaList {
			if !meta.Enabled {
				continue
			}
			if len(meta.ReqSubkeys) > 0 && !rules.DefaultSubKeysInEvent(meta, event) {
				continue
			}
			e, exists := byRule[meta.Id]
			if !exists {
				e = &ruleEntry{meta: meta}
				byRule[meta.Id] = e
				ruleOrder = append(ruleOrder, e)
			}
			e.events = append(e.events, event)
		}
	}

	if len(byRule) == 0 {
		return nil
	}

	rulesPerBatch.Observe(float64(len(byRule)))
	s.logger.Info("evaluating %d rule(s) across batch of %d message(s)", len(byRule), len(msgs))

	// Evaluate every rule independently. The rule pool owns tenant-sticky rollout
	// partitioning. No alert is built or published until every rule completes.
	evalCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var batchErr errors.Error
	setBatchErr := func(err errors.Error) {
		mu.Lock()
		defer mu.Unlock()
		if batchErr == nil {
			batchErr = err
			cancel()
		}
	}
	for _, entry := range ruleOrder {
		wg.Add(1)
		go func(entry *ruleEntry) {
			defer wg.Done()
			if err := s.sem.Acquire(evalCtx, 1); err != nil {
				setBatchErr(errors.NewE(err))
				return
			}
			concurrencyGauge.Inc()
			defer func() {
				s.sem.Release(1)
				concurrencyGauge.Dec()
			}()

			cctx, cancel := context.WithTimeout(evalCtx, time.Duration(s.config.TimeoutSec)*time.Second)
			defer cancel()
			results, err := s.evaluateRule(cctx, entry.meta, entry.events)
			if err != nil {
				setBatchErr(err)
				return
			}
			entry.results = results
		}(entry)
	}
	wg.Wait()
	if batchErr != nil {
		return batchErr
	}

	for _, entry := range ruleOrder {
		if err := s.publishRuleResults(ctx, entry, entry.results); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) evaluateRule(ctx context.Context, meta *rules.RuleMetadata, events []events.Event) ([]rules.EvaluateItem, errors.Error) {
	startEval := time.Now()
	eval := s.pool.Evaluate(ctx, meta.Id, events)
	ruleEvalHist.WithLabelValues(meta.Name).Observe(time.Since(startEval).Seconds())
	if eval.CallErr != nil {
		ruleEvalErrors.WithLabelValues(meta.Name).Inc()
		return nil, eval.CallErr
	}
	if len(eval.Items) != len(events) {
		return nil, errors.NewF("rule %s returned %d items for %d events", meta.Name, len(eval.Items), len(events))
	}
	results := make([]rules.EvaluateItem, len(eval.Items))
	for i, item := range eval.Items {
		if item.Err != nil {
			ruleEvalErrors.WithLabelValues(meta.Name).Inc()
			return nil, item.Err
		}
		results[i] = item
	}
	return results, nil
}

func (s *Service) publishRuleResults(ctx context.Context, entry *ruleEntry, results []rules.EvaluateItem) errors.Error {
	for i, result := range results {
		if !result.Matched {
			continue
		}
		alert, err := alerts.NewAlert(entry.meta, mergeEventContext(entry.events[i], result.Context))
		if err != nil {
			return err
		}

		// Apply optional per-event overrides from the plugin.
		if len(result.MergeByKeys) > 0 {
			alert.OverrideMergeByKeys = result.MergeByKeys
		}
		if result.Severity != "" {
			if sev, err := scoring.ParseSeverity(result.Severity); err == nil {
				alert.Severity = sev
			} else {
				return errors.NewF("rule %s returned invalid severity %q: %v", entry.meta.Name, result.Severity, err)
			}
		}

		payload, marshalErr := alerts.Marshal(alert)
		if marshalErr != nil {
			return errors.NewE(marshalErr)
		}
		startWrite := time.Now()
		if err := s.mergerWriter.WriteMessages(ctx, brokers.Message{
			Key:   []byte(alert.MergePartitionKey()),
			Value: payload,
		}); err != nil {
			alertsWriteErrors.Inc()
			return errors.NewE(err)
		}
		alertsWriteDuration.Observe(time.Since(startWrite).Seconds())
		ruleMatches.WithLabelValues(entry.meta.Name).Inc()
		alertsOut.Inc()
	}
	return nil
}

// eligibleRules returns the rule metadata to evaluate for this event.
func (s *Service) eligibleRules(ruleSet []*rules.RuleMetadata, logType string, ruleIDs []string) ([]*rules.RuleMetadata, errors.Error) {
	all := rules.RulesForLogTypeIn(ruleSet, logType)
	if len(ruleIDs) == 0 {
		return all, nil
	}

	idSet := make(map[string]struct{}, len(ruleIDs))
	for _, id := range ruleIDs {
		idSet[id] = struct{}{}
	}

	var result []*rules.RuleMetadata
	for _, meta := range all {
		if _, ok := idSet[meta.Id]; ok {
			result = append(result, meta)
			delete(idSet, meta.Id)
		}
	}
	for id := range idSet {
		return nil, errors.NewF("explicit rule %s is unavailable for log type %s", id, logType)
	}
	return result, nil
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

package executor

import (
	"context"
	stderrors "errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/exec/pb"
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

	dlqOut         = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "dlq_records_total"}, []string{"stage"})
	dlqWriteErrors = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "dlq_write_errors_total"})

	ruleMatches = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_matches_total"}, []string{"rule"})
)

type ruleItem struct {
	event    events.Event
	source   brokers.Message
	result   rules.EvaluateItem
	attempts int
	alert    brokers.Message
}

type ruleEntry struct {
	meta  *rules.RuleMetadata
	items []ruleItem
}

// Reads ExecMessages from blink-exec, applies the rolled out rules, and writes alerts to blink-merger.
type Service struct {
	logger       *logger.Logger
	config       Config
	mergerWriter brokers.Writer
	dlqWriter    brokers.Writer
	ruleCfg      *rules.SnapshotConfig
	pool         *rules.Pool
	sem          *semaphore.Weighted
}

// Config contains the environment-loaded settings and runtime dependencies injected by main.
type Config struct {
	Broker        brokers.Broker
	ExecutorTopic string `env:"KAFKA_TOPIC_EXECUTOR"`
	ExecutorGroup string `env:"KAFKA_GROUP_EXECUTOR"`
	MergerTopic   string `env:"KAFKA_TOPIC_MERGER"`
	DLQTopic      string `env:"KAFKA_TOPIC_EXECUTOR_DLQ"`
	// ReadyFn gates grouped-consumer creation until the rule snapshot catch-up completes.
	ReadyFn func() bool
	// BatchSize is the number of messages to read from the broker at once.
	BatchSize int `env:"EXECUTOR_BATCH_SIZE,optional"`
	// Concurrency is the maximum number of concurrent rule pool calls.
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

// batch is one poll's decoded work: ordered rule entries plus dead-letter records.
type batch struct {
	entries []*ruleEntry
	dlq     []brokers.Message
}

func NewService(logger *logger.Logger, cfg Config, pool *rules.Pool, ruleCfg *rules.SnapshotConfig) *Service {
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

func (s *Service) Run(ctx context.Context) errors.Error {
	if !waitForReady(ctx, s.config.ReadyFn) {
		return nil
	}

	s.logger.Info("catalogs ready; consuming events (topic=%s group=%s)", s.config.ExecutorTopic, s.config.ExecutorGroup)
	reader := s.config.Broker.NewReader(s.config.ExecutorTopic, s.config.ExecutorGroup)
	defer func() {
		if err := reader.Close(); err != nil {
			s.logger.Error(errors.NewE(err))
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
			err := errors.NewE(err)
			s.logger.Error(err)
			return err
		}
		batchSizeHist.Observe(float64(len(msgs)))
		eventsIn.Add(float64(len(msgs)))

		if err := s.processBatch(ctx, msgs); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.logger.Error(err)
			return err
		}

		startCommit := time.Now()
		if err := reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			commitErrors.Inc()
			err := errors.NewE(err)
			s.logger.Error(err)
			return err
		}
		commitDuration.Observe(time.Since(startCommit).Seconds())
		batchProcessDuration.Observe(time.Since(batchStart).Seconds())
	}
}

// waitForReady polls the optional snapshot readiness callback before reader creation.
func waitForReady(ctx context.Context, readyFn func() bool) bool {
	if readyFn == nil {
		return true
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if readyFn() {
			return ctx.Err() == nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// processBatch decodes, evaluates, prepares, and publishes one fetched batch.
func (s *Service) processBatch(ctx context.Context, msgs []brokers.Message) errors.Error {
	batch := s.decode(msgs)
	if len(batch.entries) > 0 {
		rulesPerBatch.Observe(float64(len(batch.entries)))
		s.logger.Info("evaluating %d rule(s) across batch of %d message(s)", len(batch.entries), len(msgs))
		s.evaluateRules(ctx, batch.entries)
		if ctx.Err() != nil {
			err := errors.NewE(ctx.Err())
			s.logger.Error(err)
			return err
		}
	}
	if err := s.prepare(batch); err != nil {
		s.logger.Error(err)
		return err
	}
	return s.publish(ctx, batch)
}

// decode turns raw records into per-rule work
func (s *Service) decode(msgs []brokers.Message) *batch {
	ruleSet := s.ruleCfg.Primaries()
	batch := &batch{}
	byRule := make(map[string]*ruleEntry)
	for _, msg := range msgs {
		var execMsg pb.ExecMessage
		if err := proto.Unmarshal(msg.Value, &execMsg); err != nil {
			eventsParseErrors.Inc()
			s.queueDLQ(batch, msg, "decode", err.Error(), 0)
			continue
		}
		if execMsg.GetEvent() == nil {
			eventsParseErrors.Inc()
			s.queueDLQ(batch, msg, "decode", "exec message has no event", 0)
			continue
		}

		event := execMsg.GetEvent().AsMap()
		logType, ok := event["log_type"].(string)
		if !ok {
			eventsInvalidLogType.Inc()
			s.queueDLQ(batch, msg, "log_type", "event log_type must be a string", 0)
			continue
		}

		metaList, err := eligibleRules(ruleSet, logType, execMsg.GetRuleIds())
		if err != nil {
			s.queueDLQ(batch, msg, "rules", err.Error(), 0)
			continue
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
			entry, exists := byRule[meta.Id]
			if !exists {
				entry = &ruleEntry{meta: meta}
				byRule[meta.Id] = entry
				batch.entries = append(batch.entries, entry)
			}
			entry.items = append(entry.items, ruleItem{event: event, source: msg})
		}
	}
	return batch
}

// evaluateRules runs rule retry loops concurrently while evaluate bounds active pool calls.
func (s *Service) evaluateRules(ctx context.Context, entries []*ruleEntry) {
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Go(func() {
			s.evaluateWithRetries(ctx, entry)
		})
	}
	wg.Wait()
}

var errPendingRetries = stderrors.New("rule retries pending")

// evaluateWithRetries retries only failed items up to MaxAttempts, preserving successful results.
func (s *Service) evaluateWithRetries(ctx context.Context, entry *ruleEntry) {
	pendingItems := make([]*ruleItem, len(entry.items))
	for i := range entry.items {
		pendingItems[i] = &entry.items[i]
	}

	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		pendingEvents := make([]events.Event, len(pendingItems))
		for i, item := range pendingItems {
			pendingEvents[i] = item.event
		}

		evaluation := s.evaluate(ctx, entry.meta, pendingEvents)
		if evaluation.CallErr != nil {
			if attempt == s.config.MaxAttempts {
				for _, item := range pendingItems {
					item.result = rules.EvaluateItem{Err: evaluation.CallErr}
					item.attempts = attempt
				}
				return nil
			}
			return errPendingRetries
		}

		next := make([]*ruleItem, 0, len(pendingItems))
		for i, item := range pendingItems {
			result := evaluation.Items[i]
			if result.Err == nil {
				item.result = result
				continue
			}
			if attempt == s.config.MaxAttempts {
				item.result = result
				item.attempts = attempt
			} else {
				next = append(next, item)
			}
		}
		pendingItems = next
		if len(pendingItems) == 0 {
			return nil
		}
		return errPendingRetries
	}, s.newBackoff(ctx))
}

func (s *Service) evaluate(ctx context.Context, rule *rules.RuleMetadata, events []events.Event) rules.EvaluateResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return rules.EvaluateResult{CallErr: errors.NewE(err)}
	}
	concurrencyGauge.Inc()
	defer func() {
		s.sem.Release(1)
		concurrencyGauge.Dec()
	}()

	evalCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	startEval := time.Now()
	result := s.pool.Evaluate(evalCtx, rule.Id, events)
	ruleEvalHist.WithLabelValues(rule.Name).Observe(time.Since(startEval).Seconds())
	if result.CallErr != nil {
		ruleEvalErrors.WithLabelValues(rule.Name).Inc()
		return result
	}
	if len(result.Items) != len(events) {
		ruleEvalErrors.WithLabelValues(rule.Name).Inc()
		return rules.EvaluateResult{CallErr: errors.NewF("rule %s returned %d items for %d events", rule.Name, len(result.Items), len(events))}
	}
	itemErrors := 0
	for _, item := range result.Items {
		if item.Err != nil {
			itemErrors++
		}
	}
	if itemErrors > 0 {
		ruleEvalErrors.WithLabelValues(rule.Name).Add(float64(itemErrors))
	}
	return result
}

// prepare builds every alert and dead-letter record before any broker write begins.
func (s *Service) prepare(batch *batch) errors.Error {
	for _, entry := range batch.entries {
		for i := range entry.items {
			item := &entry.items[i]
			if item.result.Err != nil {
				// ponytail: N failing rules yield N DLQs; dedupe by source offset only if volume warrants it.
				reason := fmt.Sprintf("rule %s: %s", entry.meta.Name, item.result.Err)
				s.queueDLQ(batch, item.source, "rule", reason, item.attempts)
				continue
			}
			if !item.result.Matched {
				continue
			}

			alert, err := alerts.NewAlert(entry.meta, mergeEventContext(item.event, item.result.Context))
			if err != nil {
				return err
			}
			if len(item.result.MergeByKeys) > 0 {
				alert.OverrideMergeByKeys = item.result.MergeByKeys
			}
			if item.result.Severity != "" {
				sev, err := scoring.ParseSeverity(item.result.Severity)
				if err != nil {
					return errors.NewF("rule %s returned invalid severity %q: %v", entry.meta.Name, item.result.Severity, err)
				}
				alert.Severity = sev
			}
			payload, marshalErr := alerts.Marshal(alert)
			if marshalErr != nil {
				return errors.NewE(marshalErr)
			}
			item.alert = brokers.Message{Key: []byte(alert.MergePartitionKey()), Value: payload}
		}
	}
	return nil
}

// publish flushes prepared alerts, then dead-letter records, before the caller commits offsets.
func (s *Service) publish(ctx context.Context, batch *batch) errors.Error {
	for _, entry := range batch.entries {
		for i := range entry.items {
			item := &entry.items[i]
			if item.result.Err != nil || !item.result.Matched {
				continue
			}
			startWrite := time.Now()
			if err := s.writeWithRetries(ctx, s.mergerWriter, item.alert, alertsWriteErrors); err != nil {
				return err
			}
			alertsWriteDuration.Observe(time.Since(startWrite).Seconds())
			ruleMatches.WithLabelValues(entry.meta.Name).Inc()
			alertsOut.Inc()
		}
	}
	for _, rec := range batch.dlq {
		if err := s.writeWithRetries(ctx, s.dlqWriter, rec, dlqWriteErrors); err != nil {
			return err
		}
	}
	return nil
}

// writeWithRetries retries a publish until it succeeds or the context is canceled.
func (s *Service) writeWithRetries(ctx context.Context, w brokers.Writer, msg brokers.Message, errCount prometheus.Counter) errors.Error {
	err := backoff.Retry(func() error {
		if werr := w.WriteMessages(ctx, msg); werr != nil {
			errCount.Inc()
			return werr
		}
		return nil
	}, s.newBackoff(ctx))
	if err != nil {
		return errors.NewE(err)
	}
	return nil
}

// queueDLQ appends a serialized DLQ record or drops an envelope that cannot be serialized.
func (s *Service) queueDLQ(batch *batch, source brokers.Message, stage, reason string, attempts int) {
	msg, err := dlq.Record(source, stage, reason, attempts)
	if err != nil {
		s.logger.ErrorF("dropping dead-letter record (stage=%s): %v", stage, err)
		return
	}
	dlqOut.WithLabelValues(stage).Inc()
	batch.dlq = append(batch.dlq, msg)
}

// newBackoff returns the service's exponential retry policy (RetryBaseMS initial, RetryCapMS cap, jittered).
func (s *Service) newBackoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(time.Duration(s.config.RetryBaseMS)*time.Millisecond),
		backoff.WithMaxInterval(time.Duration(s.config.RetryCapMS)*time.Millisecond),
		backoff.WithMaxElapsedTime(0),
	)
	return backoff.WithContext(b, ctx)
}

// eligibleRules returns the rule metadata to evaluate for this event.
func eligibleRules(ruleSet []*rules.RuleMetadata, logType string, ruleIDs []string) ([]*rules.RuleMetadata, errors.Error) {
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

// mergeEventContext overlays context on a copy of the shared event and returns the original when context is empty.
func mergeEventContext(event events.Event, ctx map[string]any) events.Event {
	if len(ctx) == 0 {
		return event
	}
	merged := make(events.Event, len(event)+len(ctx))
	maps.Copy(merged, event)
	maps.Copy(merged, ctx)
	return merged
}

package executor

import (
	"context"
	stderrors "errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/exec/pb"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
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
	batchSizeHist        = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_size"})
	eventsIn             = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_in_total"})
	alertsOut            = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_out_total"})
	ruleEvalHist         = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_evaluation_seconds"}, []string{"rule"})
	ruleEvalErrors       = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_evaluation_errors_total"}, []string{"rule"})
	readBatchErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_errors_total"})
	readBatchDuration    = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_seconds"})
	commitErrors         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_errors_total"})
	commitDuration       = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_seconds"})
	eventsParseErrors    = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_parse_errors_total"})
	eventsInvalidLogType = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_invalid_log_type_total"})
	eventsNoRules        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_no_rules_total"})
	batchProcessDuration = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_processing_seconds"})
	rulesPerBatch        = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rules_per_batch"})
	concurrencyGauge     = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "concurrent_rules"})
	alertsWriteErrors    = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_errors_total"})
	alertsWriteDuration  = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "alerts_write_seconds"})
	dlqOut               = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "dlq_records_total"}, []string{"stage"})
	dlqWriteErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "dlq_write_errors_total"})
	ruleMatches          = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_matches_total"}, []string{"rule"})
)

const (
	readinessInterval = 500 * time.Millisecond
	readinessTimeout  = time.Second
	readinessGrace    = 2 * time.Second
)

type ruleItem struct {
	event    events.Event
	raw      []byte
	source   brokers.Message
	result   rules.EvaluateItem
	attempts int
	alert    brokers.Message
}

type ruleEntry struct {
	meta  *rules.RuleMetadata
	items []ruleItem
}

// Service evaluates routed events and publishes alerts or dead-letter records.
type Service struct {
	logger         *logger.Logger
	config         Config
	mergerWriter   brokers.Writer
	dlqWriter      brokers.Writer
	runtime        RuleRuntime
	sem            *semaphore.Weighted
	ready          atomic.Bool
	snapshotsReady atomic.Bool
	readyAt        atomic.Int64
}

// RuleRuntime supplies committed rule state and evaluates events against it.
type RuleRuntime interface {
	State(context.Context) (snapshot.ProjectionState[*rules.RuleMetadata], error)
	Status(context.Context) (plugin.SupervisorStatus, error)
	Evaluate(context.Context, snapshot.ProjectionState[*rules.RuleMetadata], string, *events.Batch) rules.EvaluateResult
}

// Config contains the environment-loaded settings and runtime dependencies injected by main.
type Config struct {
	Broker        brokers.Broker
	ExecutorTopic string `env:"KAFKA_TOPIC_EXECUTOR"`
	ExecutorGroup string `env:"KAFKA_GROUP_EXECUTOR"`
	MergerTopic   string `env:"KAFKA_TOPIC_MERGER"`
	DLQTopic      string `env:"KAFKA_TOPIC_EXECUTOR_DLQ"`
	BatchSize     int    `env:"EXECUTOR_BATCH_SIZE,optional"`
	Concurrency   int    `env:"EXECUTOR_CONCURRENCY,optional"`
	TimeoutSec    int    `env:"EXECUTOR_TIMEOUT_SEC,optional"`
	MaxAttempts   int    `env:"EXECUTOR_MAX_ATTEMPTS,optional"`
	RetryBaseMS   int    `env:"EXECUTOR_RETRY_BASE_MS,optional"`
	RetryCapMS    int    `env:"EXECUTOR_RETRY_CAP_MS,optional"`
}

// batch is one poll's decoded work: ordered rule entries plus dead-letter records.
type batch struct {
	entries []*ruleEntry
	dlq     []brokers.Message
}

// WithDefaults fills optional settings and ensures the retry cap is at least the base delay.
func (c Config) WithDefaults() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = 10000
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 10
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = 10
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.RetryBaseMS <= 0 {
		c.RetryBaseMS = 100
	}
	if c.RetryCapMS <= 0 {
		c.RetryCapMS = 5000
	}
	if c.RetryCapMS < c.RetryBaseMS {
		c.RetryCapMS = c.RetryBaseMS
	}
	return c
}

func NewService(logger *logger.Logger, cfg Config, runtime RuleRuntime) *Service {
	cfg = cfg.WithDefaults()

	return &Service{
		logger:       logger,
		config:       cfg,
		mergerWriter: cfg.Broker.NewWriter(cfg.MergerTopic),
		dlqWriter:    cfg.Broker.NewWriter(cfg.DLQTopic),
		runtime:      runtime,
		sem:          semaphore.NewWeighted(int64(cfg.Concurrency)),
	}
}

// Name identifies the service to the Runner.
func (s *Service) Name() string { return "rule-executor" }

// Ready reports cached readiness without issuing runtime calls for each probe.
func (s *Service) Ready() bool { return s.ready.Load() && s.snapshotsReady.Load() }

// Run consumes batches until failure or cancellation, leaving unfinished batches uncommitted.
func (s *Service) Run(ctx context.Context) errors.Error {
	s.ready.Store(false)
	s.snapshotsReady.Store(false)
	s.readyAt.Store(0)
	defer s.ready.Store(false)

	pollCtx, stopPolling := context.WithCancel(ctx)
	pollingDone := make(chan struct{})
	defer func() {
		stopPolling()
		<-pollingDone
	}()
	go func() {
		defer close(pollingDone)
		s.pollReadiness(pollCtx)
	}()
	s.ready.Store(true)

	if err := s.waitForReady(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.NewE(err)
	}
	s.readyAt.Store(time.Now().UnixNano())
	s.snapshotsReady.Store(true)

	s.logger.Info("runtime ready; consuming events (topic=%s group=%s)", s.config.ExecutorTopic, s.config.ExecutorGroup)
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

// waitForReady requires a nonempty ready rule snapshot and ready runtime before consumption.
func (s *Service) waitForReady(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, stateErr := s.runtime.State(ctx)
		status, statusErr := s.runtime.Status(ctx)
		if stateErr == nil && statusErr == nil &&
			state.Availability == runtime.AvailabilityReady && len(state.Primaries) > 0 &&
			status.Availability == runtime.AvailabilityReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollReadiness refreshes cached state readiness and allows a brief stale-state grace period.
func (s *Service) pollReadiness(ctx context.Context) {
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()
	for {
		callCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
		state, err := s.runtime.State(callCtx)
		cancel()
		if err == nil && state.Availability == runtime.AvailabilityReady {
			s.readyAt.Store(time.Now().UnixNano())
			s.snapshotsReady.Store(true)
		} else if time.Since(time.Unix(0, s.readyAt.Load())) > readinessGrace {
			s.snapshotsReady.Store(false)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// processBatch decodes, evaluates, prepares, and publishes one fetched batch.
func (s *Service) processBatch(ctx context.Context, msgs []brokers.Message) errors.Error {
	ruleState, err := s.runtime.State(ctx)
	if err != nil {
		return errors.NewE(err)
	}
	batch := s.decode(msgs, ruleState.Primaries)
	if len(batch.entries) > 0 {
		rulesPerBatch.Observe(float64(len(batch.entries)))
		s.logger.Info("evaluating %d rule(s) across batch of %d message(s)", len(batch.entries), len(msgs))
		s.evaluateRules(ctx, ruleState, batch.entries)
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

// decode turns raw records into per-rule work and queues invalid records for the DLQ.
func (s *Service) decode(msgs []brokers.Message, ruleSet []*rules.RuleMetadata) *batch {
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

		// Encode each event once and share the bytes across eligible rules.
		raw, marshalErr := proto.Marshal(execMsg.GetEvent())
		if marshalErr != nil {
			eventsParseErrors.Inc()
			s.queueDLQ(batch, msg, "encode", marshalErr.Error(), 0)
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
			entry.items = append(entry.items, ruleItem{event: event, raw: raw, source: msg})
		}
	}
	return batch
}

// evaluateRules runs rule retry loops concurrently while evaluate bounds active runtime calls.
func (s *Service) evaluateRules(ctx context.Context, state snapshot.ProjectionState[*rules.RuleMetadata], entries []*ruleEntry) {
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Go(func() {
			s.evaluateWithRetries(ctx, state, entry)
		})
	}
	wg.Wait()
}

var errPendingRetries = stderrors.New("rule retries pending")

// evaluateWithRetries retries only failed items up to MaxAttempts, preserving successful results.
func (s *Service) evaluateWithRetries(ctx context.Context, state snapshot.ProjectionState[*rules.RuleMetadata], entry *ruleEntry) {
	pendingItems := make([]*ruleItem, len(entry.items))
	for i := range entry.items {
		pendingItems[i] = &entry.items[i]
	}

	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		pendingEvents := make([]events.Event, len(pendingItems))
		pendingRaw := make([][]byte, len(pendingItems))
		for i, item := range pendingItems {
			pendingEvents[i] = item.event
			pendingRaw[i] = item.raw
		}

		evaluation := s.evaluate(ctx, state, entry.meta, events.NewBatch(pendingEvents, pendingRaw))
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

func (s *Service) evaluate(ctx context.Context, state snapshot.ProjectionState[*rules.RuleMetadata], rule *rules.RuleMetadata, batch *events.Batch) rules.EvaluateResult {
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
	result := s.runtime.Evaluate(evalCtx, state, rule.Id, batch)
	ruleEvalHist.WithLabelValues(rule.Name).Observe(time.Since(startEval).Seconds())
	if result.CallErr != nil {
		ruleEvalErrors.WithLabelValues(rule.Name).Inc()
		return result
	}
	if len(result.Items) != batch.Len() {
		ruleEvalErrors.WithLabelValues(rule.Name).Inc()
		return rules.EvaluateResult{CallErr: errors.NewF("rule %s returned %d items for %d events", rule.Name, len(result.Items), batch.Len())}
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

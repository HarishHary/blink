package executor

import (
	"context"
	stderrors "errors"
	"fmt"
	"maps"
	"slices"
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
	durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	batchBuckets    = []float64{0, 1, 10, 50, 100, 500, 1000, 5000, 10000, 50000}
	eventsIn        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "events_in_total", Help: "Records returned by successful broker batch reads, including invalid records."})
	batchSize       = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_size", Help: "Records returned by each successful broker batch read.", Buckets: batchBuckets})
	readBatchTotal  = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_total", Help: "Broker batch read attempts by result."}, []string{"result"})
	readBatchTime   = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "read_batch_seconds", Help: "Duration of broker batch read attempts.", Buckets: durationBuckets})
	batchTotal      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_processing_total", Help: "Fetched batch processing attempts by result, excluding broker reads and commits."}, []string{"result"})
	batchTime       = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "batch_processing_seconds", Help: "Duration of fetched batch processing, excluding broker reads and commits.", Buckets: durationBuckets})
	commitTotal     = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_total", Help: "Broker commit attempts by result."}, []string{"result"})
	commitTime      = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "commit_seconds", Help: "Duration of broker commit attempts.", Buckets: durationBuckets})
	evaluationTotal = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "evaluation_total", Help: "Admitted rule runtime call attempts by plugin and result."}, []string{"plugin", "result"})
	evaluationTime  = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "evaluation_seconds", Help: "Rule runtime call duration after admission, excluding retry backoff.", Buckets: durationBuckets}, []string{"plugin"})
	evaluationItems = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "evaluation_items_total", Help: "Rule item attempts by plugin and result."}, []string{"plugin", "result"})
	evaluationRetry = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "evaluation_retries_total", Help: "Additional admitted rule runtime calls after the first call in a retry loop."}, []string{"plugin"})
	evaluationsLive = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "evaluations_in_flight", Help: "Rule runtime calls currently admitted."})
	writeTotal      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "write_total", Help: "Broker write attempts by destination and result."}, []string{"destination", "result"})
	writeTime       = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "write_seconds", Help: "Duration of broker write attempts by destination.", Buckets: durationBuckets}, []string{"destination"})
	writeRetry      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "write_retries_total", Help: "Additional broker write attempts after the first call in a retry loop."}, []string{"destination"})
	recordsOut      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "records_out_total", Help: "Records acknowledged by broker writes by destination."}, []string{"destination"})
	dlqRecords      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "dlq_records_total", Help: "Dead-letter records acknowledged by broker writes by processing stage."}, []string{"stage"})
	drops           = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "drops_total", Help: "Drop decisions by scope and reason; may include decisions from batches that are not committed."}, []string{"scope", "reason"})
	rulesPerBatch   = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rules_per_batch", Help: "Distinct rules evaluated for each fetched batch.", Buckets: []float64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000}})
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
	dlq     []preparedDLQ
}

type preparedDLQ struct {
	message brokers.Message
	stage   string
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
		start := time.Now()
		msgs, err := reader.ReadBatch(ctx, s.config.BatchSize)
		readBatchTime.Observe(time.Since(start).Seconds())
		readBatchTotal.WithLabelValues(metricResult(ctx, err)).Inc()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			err := errors.NewE(err)
			s.logger.Error(err)
			return err
		}
		batchSize.Observe(float64(len(msgs)))
		eventsIn.Add(float64(len(msgs)))

		start = time.Now()
		batchErr := s.processBatch(ctx, msgs)
		batchTime.Observe(time.Since(start).Seconds())
		batchTotal.WithLabelValues(metricResult(ctx, batchErr)).Inc()
		if batchErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.logger.Error(batchErr)
			return batchErr
		}

		start = time.Now()
		commitErr := reader.CommitMessages(ctx, msgs...)
		commitTime.Observe(time.Since(start).Seconds())
		commitTotal.WithLabelValues(metricResult(ctx, commitErr)).Inc()
		if commitErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			err := errors.NewE(commitErr)
			s.logger.Error(err)
			return err
		}
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
	rulesPerBatch.Observe(float64(len(batch.entries)))
	if len(batch.entries) > 0 {
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
			s.queueDLQ(batch, msg, "decode", err.Error(), 0)
			continue
		}
		if execMsg.GetEvent() == nil {
			s.queueDLQ(batch, msg, "decode", "exec message has no event", 0)
			continue
		}

		event := execMsg.GetEvent().AsMap()
		logType, ok := event["log_type"].(string)
		if !ok {
			s.queueDLQ(batch, msg, "log_type", "event log_type must be a string", 0)
			continue
		}
		if len(execMsg.GetRuleIds()) == 0 {
			for _, meta := range ruleSet {
				if !meta.Enabled && (len(meta.LogTypes) == 0 || slices.Contains(meta.LogTypes, logType)) {
					drops.WithLabelValues("rule", "disabled").Inc()
				}
			}
		}

		metaList, err := eligibleRules(ruleSet, logType, execMsg.GetRuleIds())
		if err != nil {
			s.queueDLQ(batch, msg, "rules", err.Error(), 0)
			continue
		}
		if len(metaList) == 0 {
			drops.WithLabelValues("event", "no_rules").Inc()
			continue
		}

		// Encode each event once and share the bytes across eligible rules.
		raw, marshalErr := proto.Marshal(execMsg.GetEvent())
		if marshalErr != nil {
			s.queueDLQ(batch, msg, "encode", marshalErr.Error(), 0)
			continue
		}

		for _, meta := range metaList {
			if !meta.Enabled {
				drops.WithLabelValues("rule", "disabled").Inc()
				continue
			}
			if len(meta.ReqSubkeys) > 0 && !rules.DefaultSubKeysInEvent(meta, event) {
				drops.WithLabelValues("rule", "missing_subkeys").Inc()
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

		evaluation := s.evaluate(ctx, state, entry.meta, events.NewBatch(pendingEvents, pendingRaw), attempt > 1)
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

func (s *Service) evaluate(ctx context.Context, state snapshot.ProjectionState[*rules.RuleMetadata], rule *rules.RuleMetadata, batch *events.Batch, retry bool) rules.EvaluateResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return rules.EvaluateResult{CallErr: errors.NewE(err)}
	}
	if retry {
		evaluationRetry.WithLabelValues(rule.Name).Inc()
	}
	evaluationsLive.Inc()
	defer func() {
		evaluationsLive.Dec()
		s.sem.Release(1)
	}()

	evalCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	startEval := time.Now()
	result := s.runtime.Evaluate(evalCtx, state, rule.Id, batch)
	evaluationTime.WithLabelValues(rule.Name).Observe(time.Since(startEval).Seconds())
	callResult := metricResult(ctx, result.CallErr)
	if len(result.Items) != batch.Len() {
		if result.CallErr == nil {
			result = rules.EvaluateResult{CallErr: errors.NewF("rule %s returned %d items for %d events", rule.Name, len(result.Items), batch.Len())}
			callResult = "error"
		}
	}
	itemErrors := 0
	if result.CallErr != nil {
		evaluationItems.WithLabelValues(rule.Name, "error").Add(float64(batch.Len()))
	} else {
		for _, item := range result.Items {
			itemResult := "unmatched"
			if item.Err != nil {
				itemResult = "error"
				itemErrors++
			} else if item.Matched {
				itemResult = "matched"
			}
			evaluationItems.WithLabelValues(rule.Name, itemResult).Inc()
		}
		if itemErrors > 0 {
			callResult = "error"
		}
	}
	evaluationTotal.WithLabelValues(rule.Name, callResult).Inc()
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
				drops.WithLabelValues("rule", "unmatched").Inc()
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
			if err := s.writeWithRetries(ctx, s.mergerWriter, "output", item.alert); err != nil {
				return err
			}
		}
	}
	for _, rec := range batch.dlq {
		if err := s.writeWithRetries(ctx, s.dlqWriter, "dlq", rec.message); err != nil {
			return err
		}
		dlqRecords.WithLabelValues(rec.stage).Inc()
	}
	return nil
}

// writeWithRetries retries a publish until it succeeds or the context is canceled.
func (s *Service) writeWithRetries(ctx context.Context, w brokers.Writer, destination string, msg brokers.Message) errors.Error {
	attempt := 0
	err := backoff.Retry(func() error {
		attempt++
		if attempt > 1 {
			writeRetry.WithLabelValues(destination).Inc()
		}
		start := time.Now()
		if werr := w.WriteMessages(ctx, msg); werr != nil {
			writeTime.WithLabelValues(destination).Observe(time.Since(start).Seconds())
			writeTotal.WithLabelValues(destination, metricResult(ctx, werr)).Inc()
			return werr
		}
		writeTime.WithLabelValues(destination).Observe(time.Since(start).Seconds())
		writeTotal.WithLabelValues(destination, "ok").Inc()
		recordsOut.WithLabelValues(destination).Inc()
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
		scope := "event"
		if stage == "rule" {
			scope = "rule"
		}
		drops.WithLabelValues(scope, "dlq_encode").Inc()
		return
	}
	batch.dlq = append(batch.dlq, preparedDLQ{message: msg, stage: stage})
}

func metricResult(ctx context.Context, err error) string {
	if err == nil {
		return "ok"
	}
	if ctx.Err() != nil {
		return "canceled"
	}
	return "error"
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

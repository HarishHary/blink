package tuner

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
	"github.com/harishhary/blink/pkg/tuning_rules"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
)

var (
	durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	batchBuckets    = []float64{0, 1, 10, 50, 100, 500, 1000, 5000, 10000, 50000}
	eventsIn        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "events_in_total", Help: "Records returned by successful broker batch reads, including invalid records."})
	batchSize       = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "batch_size", Help: "Records returned by each successful broker batch read.", Buckets: batchBuckets})
	readBatchTotal  = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "read_batch_total", Help: "Broker batch read attempts by result."}, []string{"result"})
	readBatchTime   = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "read_batch_seconds", Help: "Duration of broker batch read attempts.", Buckets: durationBuckets})
	batchTotal      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "batch_processing_total", Help: "Fetched batch processing attempts by result, excluding broker reads and commits."}, []string{"result"})
	batchTime       = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "batch_processing_seconds", Help: "Duration of fetched batch processing, excluding broker reads and commits.", Buckets: durationBuckets})
	commitTotal     = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "commit_total", Help: "Broker commit attempts by result."}, []string{"result"})
	commitTime      = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "commit_seconds", Help: "Duration of broker commit attempts.", Buckets: durationBuckets})
	evaluationTotal = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "evaluation_total", Help: "Admitted tuning-rule runtime call attempts by plugin and result."}, []string{"plugin", "result"})
	evaluationTime  = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "evaluation_seconds", Help: "Tuning-rule runtime call duration after admission, excluding retry backoff.", Buckets: durationBuckets}, []string{"plugin"})
	evaluationItems = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "evaluation_items_total", Help: "Tuning-rule item attempts by plugin and result."}, []string{"plugin", "result"})
	evaluationRetry = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "evaluation_retries_total", Help: "Additional admitted tuning-rule runtime calls after the first call in a retry loop."}, []string{"plugin"})
	evaluationsLive = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "evaluations_in_flight", Help: "Tuning-rule runtime calls currently admitted."})
	writeTotal      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "write_total", Help: "Broker write attempts by destination and result."}, []string{"destination", "result"})
	writeTime       = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "write_seconds", Help: "Duration of broker write attempts by destination.", Buckets: durationBuckets}, []string{"destination"})
	writeRetry      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "write_retries_total", Help: "Additional broker write attempts after the first call in a retry loop."}, []string{"destination"})
	recordsOut      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "records_out_total", Help: "Records acknowledged by broker writes by destination."}, []string{"destination"})
	dlqRecords      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "dlq_records_total", Help: "Dead-letter records acknowledged by broker writes by processing stage."}, []string{"stage"})
	drops           = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "drops_total", Help: "Drop decisions by scope and reason; may include decisions from batches that are not committed."}, []string{"scope", "reason"})
	alertsOut       = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_out_total", Help: "Alerts acknowledged by output broker writes by tuning result."}, []string{"result"})
)

const (
	readinessInterval = 500 * time.Millisecond
	readinessTimeout  = time.Second
	readinessGrace    = 2 * time.Second
	stateWaitMargin   = time.Second
)

type terminalKind uint8

const (
	terminalDrop terminalKind = iota
	terminalNormal
	terminalDLQ
)

type preparedRecord struct {
	kind    terminalKind
	message brokers.Message
	stage   string
}

type tuneResult struct {
	ruleType   tuning_rules.RuleType
	confidence scoring.Confidence
}

type tuningItem struct {
	state    *alertState
	meta     *tuning_rules.TuningRuleMetadata
	result   tuning_rules.TuneItem
	attempts int
}

type alertState struct {
	source            brokers.Message
	alert             *alerts.Alert
	index             int
	items             []*tuningItem
	prepared          *preparedRecord
	confidenceChanged bool
}

type tuningEntry struct {
	meta  *tuning_rules.TuningRuleMetadata
	items []*tuningItem
}

type batch struct {
	states  []*alertState
	entries []*tuningEntry
	encoded *alerts.Batch
}

// Service tunes each alert to one forwarded, ignored, or dead-letter terminal before committing its input.
type Service struct {
	logger         *logger.Logger
	config         Config
	enricherWriter brokers.Writer
	dlqWriter      brokers.Writer
	runtime        TunerRuntime
	sem            *semaphore.Weighted
	ready          atomic.Bool
	snapshotsReady atomic.Bool
	readyAt        atomic.Int64
}

// TunerRuntime supplies committed tuning-rule state and evaluates alerts against it.
type TunerRuntime interface {
	Tune(context.Context, snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata], string, *alerts.Batch) tuning_rules.TuneResult
	State(context.Context) (snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata], error)
	Status(context.Context) (plugin.SupervisorStatus, error)
}

// Config supplies service dependencies and settings.
type Config struct {
	Broker             brokers.Broker
	TunerTopic         string `env:"KAFKA_TOPIC_TUNER"`
	TunerGroup         string `env:"KAFKA_GROUP_TUNER"`
	EnricherTopic      string `env:"KAFKA_TOPIC_ENRICHER"`
	DLQTopic           string `env:"KAFKA_TOPIC_TUNER_DLQ"`
	MaxBatchSize       int    `env:"MAX_BATCH_SIZE,optional"`
	MaxConcurrentCalls int    `env:"MAX_CONCURRENT_CALLS,optional"`
	TimeoutSec         int    `env:"TIMEOUT_SEC,optional"`
	MaxAttempts        int    `env:"MAX_ATTEMPTS,optional"`
	RetryBaseMS        int    `env:"RETRY_BASE_MS,optional"`
	RetryCapMS         int    `env:"RETRY_CAP_MS,optional"`
}

// WithDefaults fills optional settings and ensures the retry cap is at least the base delay.
func (c Config) WithDefaults() Config {
	if c.MaxBatchSize <= 0 {
		c.MaxBatchSize = 10000
	}
	if c.MaxConcurrentCalls <= 0 {
		c.MaxConcurrentCalls = 10
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

func NewService(logger *logger.Logger, cfg Config, tuningRuntime TunerRuntime) *Service {
	cfg = cfg.WithDefaults()

	return &Service{
		logger:         logger,
		config:         cfg,
		enricherWriter: cfg.Broker.NewWriter(cfg.EnricherTopic),
		dlqWriter:      cfg.Broker.NewWriter(cfg.DLQTopic),
		runtime:        tuningRuntime,
		sem:            semaphore.NewWeighted(int64(cfg.MaxConcurrentCalls)),
	}
}

// Name identifies the service to the Runner.
func (s *Service) Name() string { return "rule-tuner" }

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

	s.logger.Info("runtime ready; consuming alerts (topic=%s group=%s)", s.config.TunerTopic, s.config.TunerGroup)
	reader := s.config.Broker.NewReader(s.config.TunerTopic, s.config.TunerGroup)
	defer func() {
		if err := reader.Close(); err != nil {
			s.logger.Error(errors.NewE(err))
		}
	}()

	for {
		start := time.Now()
		msgs, err := reader.ReadBatch(ctx, s.config.MaxBatchSize)
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

// waitForReady requires a ready tuning snapshot and ready runtime before consumption.
func (s *Service) waitForReady(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, stateErr := s.runtime.State(ctx)
		status, statusErr := s.runtime.Status(ctx)
		if stateErr == nil && statusErr == nil &&
			state.Availability == runtime.AvailabilityReady &&
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

// readTuningState retries unavailable state reads during the configured retry window.
func (s *Service) readTuningState(ctx context.Context) (snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata], error) {
	deadline := time.Now().Add(time.Duration(s.config.TimeoutSec)*time.Second + stateWaitMargin)
	policy := s.newBackoff(ctx)
	for {
		state, err := s.runtime.State(ctx)
		if err == nil {
			return state, nil
		}
		if !stderrors.Is(err, runtime.ErrPluginUnavailable) || time.Now().After(deadline) || !wait(ctx, policy) {
			return snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata]{}, err
		}
	}
}

// processBatch decodes, evaluates, prepares, and publishes one fetched batch.
func (s *Service) processBatch(ctx context.Context, msgs []brokers.Message) errors.Error {
	state, err := s.readTuningState(ctx)
	if err != nil {
		return errors.NewE(err)
	}
	batch := s.decode(msgs, state)
	s.evaluateTuningRules(ctx, state, batch)
	if ctx.Err() != nil {
		return errors.NewE(ctx.Err())
	}
	if err := s.prepare(batch.states); err != nil {
		return err
	}
	return s.publish(ctx, batch.states)
}

// decode turns every input into ordered alert state and groups executable work by tuning-rule ID.
func (s *Service) decode(msgs []brokers.Message, snapshotState snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata]) *batch {
	globalRules := globalTuningRules(snapshotState.Primaries)
	batch := &batch{states: make([]*alertState, len(msgs))}
	byRule := make(map[string]*tuningEntry)
	decoded := make([]*alerts.Alert, 0, len(msgs))
	raw := make([][]byte, 0, len(msgs))

	for i, msg := range msgs {
		state := &alertState{source: msg}
		batch.states[i] = state

		alert, err := alerts.Unmarshal(msg.Value)
		if err != nil {
			prepared := s.prepareDLQ(msg, "decode", err.Error(), 0)
			state.prepared = &prepared
			continue
		}
		state.alert = alert

		metaList, err := tuningRulesForAlert(snapshotState, globalRules, alert.Rule.TuningRules)
		if err != nil {
			prepared := s.prepareDLQ(msg, "tuning_rules", err.Error(), 0)
			state.prepared = &prepared
			continue
		}

		if len(metaList) == 0 {
			continue
		}
		// Encode each alert once before it is shared across tuning-rule calls.
		encoded, encodeErr := alerts.Marshal(alert)
		if encodeErr != nil {
			prepared := s.prepareDLQ(msg, "encode", encodeErr.Error(), 0)
			state.prepared = &prepared
			continue
		}
		state.index = len(decoded)
		decoded = append(decoded, alert)
		raw = append(raw, encoded)

		for _, meta := range metaList {
			item := &tuningItem{state: state, meta: meta}
			state.items = append(state.items, item)
			entry := byRule[meta.Id]
			if entry == nil {
				entry = &tuningEntry{meta: meta}
				byRule[meta.Id] = entry
				batch.entries = append(batch.entries, entry)
			}
			entry.items = append(entry.items, item)
		}
	}
	batch.encoded = alerts.NewBatch(decoded, raw)
	return batch
}

func globalTuningRules(ruleSet []*tuning_rules.TuningRuleMetadata) []*tuning_rules.TuningRuleMetadata {
	globals := make([]*tuning_rules.TuningRuleMetadata, 0)
	for _, meta := range ruleSet {
		if meta.Enabled && meta.Global {
			globals = append(globals, meta)
		}
	}
	return globals
}

// tuningRulesForAlert returns enabled global rules followed by the alert's explicit rules, deduplicated by logical ID.
func tuningRulesForAlert(cfg snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata], globals []*tuning_rules.TuningRuleMetadata, names []string) ([]*tuning_rules.TuningRuleMetadata, errors.Error) {
	result := make([]*tuning_rules.TuningRuleMetadata, 0, len(globals)+len(names))
	seen := make(map[string]struct{}, len(globals)+len(names))
	for _, meta := range globals {
		if _, ok := seen[meta.Id]; ok {
			continue
		}
		seen[meta.Id] = struct{}{}
		result = append(result, meta)
	}
	for _, name := range names {
		meta, ok := cfg.ByFileName[name]
		if !ok || !meta.Enabled {
			return nil, errors.NewF("tuning rule reference %s is unavailable", name)
		}
		if _, ok := seen[meta.Id]; ok {
			continue
		}
		seen[meta.Id] = struct{}{}
		result = append(result, meta)
	}
	return result, nil
}

func (s *Service) evaluateTuningRules(ctx context.Context, state snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata], batch *batch) {
	var wg sync.WaitGroup
	for _, entry := range batch.entries {
		wg.Go(func() {
			s.tuneWithRetries(ctx, state, batch.encoded, entry)
		})
	}
	wg.Wait()
}

var errPendingRetries = stderrors.New("tuning-rule retries pending")

// tuneWithRetries retries only failed alert-rule pairs and preserves successful outcomes.
func (s *Service) tuneWithRetries(ctx context.Context, state snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata], encoded *alerts.Batch, entry *tuningEntry) {
	pendingItems := entry.items
	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		// Gather pending alerts from the batch shared by every tuning rule.
		indexes := make([]int, len(pendingItems))
		for i, item := range pendingItems {
			indexes[i] = item.state.index
		}

		result := s.tune(ctx, state, entry.meta, encoded.Gather(indexes), attempt > 1)
		if result.CallErr != nil {
			if attempt == s.config.MaxAttempts {
				for _, item := range pendingItems {
					item.result = tuning_rules.TuneItem{Err: result.CallErr}
					item.attempts = attempt
				}
				return nil
			}
			return errPendingRetries
		}

		next := make([]*tuningItem, 0, len(pendingItems))
		for i, item := range pendingItems {
			outcome := result.Items[i]
			if outcome.Err == nil {
				item.result = outcome
				continue
			}
			if attempt == s.config.MaxAttempts {
				item.result = outcome
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

// tune performs one bounded, timed runtime call for the pending alerts.
func (s *Service) tune(ctx context.Context, state snapshot.ProjectionState[*tuning_rules.TuningRuleMetadata], meta *tuning_rules.TuningRuleMetadata, batch *alerts.Batch, retry bool) tuning_rules.TuneResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return tuning_rules.TuneResult{CallErr: errors.NewE(err)}
	}
	if retry {
		evaluationRetry.WithLabelValues(meta.Name).Inc()
	}
	evaluationsLive.Inc()
	defer func() {
		evaluationsLive.Dec()
		s.sem.Release(1)
	}()

	tuneCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	result := s.runtime.Tune(tuneCtx, state, meta.Id, batch)
	evaluationTime.WithLabelValues(meta.Name).Observe(time.Since(start).Seconds())
	callResult := metricResult(ctx, result.CallErr)
	if result.CallErr == nil && len(result.Items) != batch.Len() {
		result = tuning_rules.TuneResult{CallErr: errors.NewF("tuning rule %s returned %d items for %d alerts", meta.Name, len(result.Items), batch.Len())}
		callResult = "error"
	}
	itemErrors := 0
	if result.CallErr != nil {
		evaluationItems.WithLabelValues(meta.Name, "error").Add(float64(batch.Len()))
	} else {
		for _, item := range result.Items {
			itemResult := "unmatched"
			if item.Err != nil {
				itemResult = "error"
				itemErrors++
			} else if item.Applies {
				itemResult = "matched"
			}
			evaluationItems.WithLabelValues(meta.Name, itemResult).Inc()
		}
		if itemErrors > 0 {
			callResult = "error"
		}
	}
	evaluationTotal.WithLabelValues(meta.Name, callResult).Inc()
	return result
}

// prepare builds every forwarded alert and dead-letter record before any broker write begins.
func (s *Service) prepare(states []*alertState) errors.Error {
	for _, state := range states {
		if state.prepared != nil {
			continue
		}

		var failure *tuningItem
		results := make([]tuneResult, 0, len(state.items))
		for _, item := range state.items {
			if item.result.Err != nil {
				if failure == nil || item.meta.Id < failure.meta.Id {
					failure = item
				}
				continue
			}
			if item.result.Applies {
				results = append(results, tuneResult{ruleType: item.result.RuleType, confidence: item.result.Confidence})
			}
		}

		if failure != nil {
			reason := fmt.Sprintf("tuning rule %s: %s", failure.meta.Name, failure.result.Err.Error())
			prepared := s.prepareDLQ(state.source, "tuning_rule", reason, failure.attempts)
			state.prepared = &prepared
			continue
		}

		before := state.alert.Confidence
		confidence, ignored := applyTuningResults(before, results)
		if ignored {
			drops.WithLabelValues("event", "ignored").Inc()
			state.prepared = &preparedRecord{kind: terminalDrop}
			continue
		}
		state.alert.Confidence = confidence
		state.confidenceChanged = confidence != before
		payload, err := alerts.Marshal(state.alert)
		if err != nil {
			return errors.NewE(err)
		}
		state.prepared = &preparedRecord{kind: terminalNormal, message: brokers.Message{
			Key: append([]byte(nil), state.source.Key...), Value: payload,
		}}
	}
	return nil
}

// publish writes prepared terminals serially in fetched order before the caller commits offsets.
func (s *Service) publish(ctx context.Context, states []*alertState) errors.Error {
	for _, state := range states {
		if state.prepared == nil {
			return errors.New("rule tuner left an input without a terminal state")
		}
		switch state.prepared.kind {
		case terminalDrop:
		case terminalNormal:
			if err := s.writeWithRetries(ctx, s.enricherWriter, "output", state.prepared.message); err != nil {
				return err
			}
			result := "passthrough"
			if state.confidenceChanged {
				result = "mutated"
			}
			alertsOut.WithLabelValues(result).Inc()
		case terminalDLQ:
			if err := s.writeWithRetries(ctx, s.dlqWriter, "dlq", state.prepared.message); err != nil {
				return err
			}
			dlqRecords.WithLabelValues(state.prepared.stage).Inc()
		}
	}
	return nil
}

// writeWithRetries retries a publish indefinitely; only context cancellation stops it.
func (s *Service) writeWithRetries(ctx context.Context, writer brokers.Writer, destination string, msg brokers.Message) errors.Error {
	attempt := 0
	err := backoff.Retry(func() error {
		attempt++
		if attempt > 1 {
			writeRetry.WithLabelValues(destination).Inc()
		}
		start := time.Now()
		if writeErr := writer.WriteMessages(ctx, msg); writeErr != nil {
			writeTime.WithLabelValues(destination).Observe(time.Since(start).Seconds())
			writeTotal.WithLabelValues(destination, metricResult(ctx, writeErr)).Inc()
			return writeErr
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

// prepareDLQ returns a DLQ terminal or drops an envelope that cannot be serialized.
func (s *Service) prepareDLQ(source brokers.Message, stage, reason string, attempts int) preparedRecord {
	msg, err := dlq.Record(source, stage, reason, attempts)
	if err != nil {
		s.logger.ErrorF("dropping dead-letter record (stage=%s): %v", stage, err)
		drops.WithLabelValues("event", "dlq_encode").Inc()
		return preparedRecord{kind: terminalDrop}
	}
	return preparedRecord{kind: terminalDLQ, message: msg, stage: stage}
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

// newBackoff returns the service's exponential retry policy with the configured initial delay and cap.
func (s *Service) newBackoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(time.Duration(s.config.RetryBaseMS)*time.Millisecond),
		backoff.WithMaxInterval(time.Duration(s.config.RetryCapMS)*time.Millisecond),
		backoff.WithMaxElapsedTime(0),
	)
	return backoff.WithContext(b, ctx)
}

func wait(ctx context.Context, policy backoff.BackOff) bool {
	delay := policy.NextBackOff()
	if delay == backoff.Stop {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// applyTuningResults applies tuning results in priority order: Ignore, SetConfidence, then ordered Increase/Decrease.
func applyTuningResults(base scoring.Confidence, results []tuneResult) (scoring.Confidence, bool) {
	confidence := base
	for _, result := range results {
		if result.ruleType == tuning_rules.Ignore {
			return 0, true
		}
	}

	setByRule := false
	for _, result := range results {
		if result.ruleType == tuning_rules.SetConfidence && (!setByRule || result.confidence > confidence) {
			confidence = result.confidence
			setByRule = true
		}
	}
	if setByRule {
		return confidence, false
	}

	for _, result := range results {
		if result.ruleType == tuning_rules.IncreaseConfidence && result.confidence > confidence {
			confidence = result.confidence
		} else if result.ruleType == tuning_rules.DecreaseConfidence && result.confidence < confidence {
			confidence = result.confidence
		}
	}
	return confidence, false
}

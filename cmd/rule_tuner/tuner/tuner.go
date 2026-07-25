package tuner

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/scoring"
	"github.com/harishhary/blink/pkg/tuning_rules"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
)

var (
	alertsIn          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_in_total"})
	alertsOut         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_out_total"})
	alertsDLQ         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_dlq_total"})
	alertsIgnored     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "alerts_ignored_total"})
	confidenceChanged = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "confidence_changed_total"})
	parseErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "parse_errors_total"})
	readErrors        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "read_errors_total"})
	commitErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "commit_errors_total"})
	writeErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "write_errors_total"})
	tuningDuration    = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "tuning_duration_seconds", Buckets: prometheus.DefBuckets}, []string{"tuning_rule"})
	tuningErrors      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "tuning_errors_total"}, []string{"tuning_rule"})
	concurrencyGauge  = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "rule_tuner", Name: "concurrent_tuning_rules"})
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
	items             []*tuningItem
	prepared          *preparedRecord
	ignored           bool
	confidenceChanged bool
}

type tuningEntry struct {
	meta  *tuning_rules.TuningRuleMetadata
	items []*tuningItem
}

type batch struct {
	states  []*alertState
	entries []*tuningEntry
}

// Service tunes each alert to one forwarded, ignored, or dead-letter terminal before committing its input.
type Service struct {
	logger         *logger.Logger
	config         Config
	enricherWriter brokers.Writer
	dlqWriter      brokers.Writer
	tuningCfg      *tuning_rules.SnapshotConfig
	pool           *tuning_rules.Pool
	sem            *semaphore.Weighted
}

// Config contains the environment-loaded settings and runtime dependencies injected by main.
type Config struct {
	Broker        brokers.Broker
	TunerTopic    string `env:"KAFKA_TOPIC_TUNER"`
	TunerGroup    string `env:"KAFKA_GROUP_TUNER"`
	EnricherTopic string `env:"KAFKA_TOPIC_ENRICHER"`
	DLQTopic      string `env:"KAFKA_TOPIC_TUNER_DLQ"`
	// ReadyFn gates grouped-consumer creation until the tuning-rule snapshot catch-up completes.
	ReadyFn func() bool
	// BatchSize is the number of alerts to read from the broker at once.
	BatchSize int `env:"TUNER_BATCH_SIZE,optional"`
	// Concurrency is the maximum number of concurrent tuning-rule pool calls.
	Concurrency int `env:"TUNER_CONCURRENCY,optional"`
	// TimeoutSec bounds each tuning-rule pool call in seconds.
	TimeoutSec int `env:"TUNER_TIMEOUT_SEC,optional"`
	// MaxAttempts is how many times a failing tuning-rule call is tried per alert before DLQ.
	MaxAttempts int `env:"TUNER_MAX_ATTEMPTS,optional"`
	// RetryBaseMS is the initial tuning and publication retry delay in milliseconds.
	RetryBaseMS int `env:"TUNER_RETRY_BASE_MS,optional"`
	// RetryCapMS bounds exponential tuning and publication retry delays in milliseconds.
	RetryCapMS int `env:"TUNER_RETRY_CAP_MS,optional"`
}

func NewService(logger *logger.Logger, cfg Config, pool *tuning_rules.Pool, tuningCfg *tuning_rules.SnapshotConfig) *Service {
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
		logger:         logger,
		config:         cfg,
		enricherWriter: cfg.Broker.NewWriter(cfg.EnricherTopic),
		dlqWriter:      cfg.Broker.NewWriter(cfg.DLQTopic),
		tuningCfg:      tuningCfg,
		pool:           pool,
		sem:            semaphore.NewWeighted(int64(cfg.Concurrency)),
	}
}

func (s *Service) Name() string { return "rule-tuner" }

func (s *Service) Run(ctx context.Context) errors.Error {
	if !waitForReady(ctx, s.config.ReadyFn) {
		return nil
	}

	s.logger.Info("catalog ready; consuming alerts (topic=%s group=%s)", s.config.TunerTopic, s.config.TunerGroup)
	reader := s.config.Broker.NewReader(s.config.TunerTopic, s.config.TunerGroup)
	defer func() {
		if err := reader.Close(); err != nil {
			s.logger.Error(errors.NewE(err))
		}
	}()

	for {
		msgs, err := reader.ReadBatch(ctx, s.config.BatchSize)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			readErrors.Inc()
			err := errors.NewE(err)
			s.logger.Error(err)
			return err
		}

		if err := s.processBatch(ctx, msgs); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.logger.Error(err)
			return err
		}

		if err := reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			commitErrors.Inc()
			err := errors.NewE(err)
			s.logger.Error(err)
			return err
		}
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

func (s *Service) processBatch(ctx context.Context, msgs []brokers.Message) errors.Error {
	batch := s.decode(msgs)
	s.evaluateTuningRules(ctx, batch.entries)
	if ctx.Err() != nil {
		return errors.NewE(ctx.Err())
	}
	if err := s.prepare(batch.states); err != nil {
		return err
	}
	return s.publish(ctx, batch.states)
}

// decode turns every input into ordered alert state and groups executable work by tuning-rule ID.
func (s *Service) decode(msgs []brokers.Message) *batch {
	globalRules := globalTuningRules(s.tuningCfg.Primaries())
	batch := &batch{states: make([]*alertState, len(msgs))}
	byRule := make(map[string]*tuningEntry)

	for i, msg := range msgs {
		state := &alertState{source: msg}
		batch.states[i] = state

		alert, err := alerts.Unmarshal(msg.Value)
		if err != nil {
			parseErrors.Inc()
			prepared := s.prepareDLQ(msg, "decode", err.Error(), 0)
			state.prepared = &prepared
			continue
		}
		alertsIn.Inc()
		state.alert = alert

		metaList, err := tuningRulesForAlert(s.tuningCfg, globalRules, alert.Rule.TuningRules)
		if err != nil {
			prepared := s.prepareDLQ(msg, "tuning_rules", err.Error(), 0)
			state.prepared = &prepared
			continue
		}

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
func tuningRulesForAlert(cfg *tuning_rules.SnapshotConfig, globals []*tuning_rules.TuningRuleMetadata, names []string) ([]*tuning_rules.TuningRuleMetadata, errors.Error) {
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
		meta, ok := cfg.ByFileName(name)
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

func (s *Service) evaluateTuningRules(ctx context.Context, entries []*tuningEntry) {
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Go(func() {
			s.tuneWithRetries(ctx, entry)
		})
	}
	wg.Wait()
}

var errPendingRetries = stderrors.New("tuning-rule retries pending")

// tuneWithRetries retries only failed alert-rule pairs and preserves successful outcomes.
func (s *Service) tuneWithRetries(ctx context.Context, entry *tuningEntry) {
	pendingItems := entry.items
	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		pendingAlerts := make([]alerts.Alert, len(pendingItems))
		for i, item := range pendingItems {
			pendingAlerts[i] = *item.state.alert.Clone()
		}

		result := s.tune(ctx, entry.meta, pendingAlerts)
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

// tune performs one bounded, timed Pool.Tune call for the pending alerts.
func (s *Service) tune(ctx context.Context, meta *tuning_rules.TuningRuleMetadata, alerts []alerts.Alert) tuning_rules.TuneResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return tuning_rules.TuneResult{CallErr: errors.NewE(err)}
	}
	concurrencyGauge.Inc()
	defer func() {
		s.sem.Release(1)
		concurrencyGauge.Dec()
	}()

	tuneCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	result := s.pool.Tune(tuneCtx, meta.Id, alerts)
	tuningDuration.WithLabelValues(meta.Name).Observe(time.Since(start).Seconds())
	if result.CallErr != nil {
		tuningErrors.WithLabelValues(meta.Name).Inc()
		return result
	}
	if len(result.Items) != len(alerts) {
		tuningErrors.WithLabelValues(meta.Name).Inc()
		return tuning_rules.TuneResult{CallErr: errors.NewF("tuning rule %s returned %d items for %d alerts", meta.Name, len(result.Items), len(alerts))}
	}
	itemErrors := 0
	for _, item := range result.Items {
		if item.Err != nil {
			itemErrors++
		}
	}
	if itemErrors > 0 {
		tuningErrors.WithLabelValues(meta.Name).Add(float64(itemErrors))
	}
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
			state.ignored = true
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
			if state.ignored {
				alertsIgnored.Inc()
			}
		case terminalNormal:
			if err := s.writeWithRetries(ctx, s.enricherWriter, state.prepared.message); err != nil {
				return err
			}
			alertsOut.Inc()
			if state.confidenceChanged {
				confidenceChanged.Inc()
			}
		case terminalDLQ:
			if err := s.writeWithRetries(ctx, s.dlqWriter, state.prepared.message); err != nil {
				return err
			}
			alertsDLQ.Inc()
		}
	}
	return nil
}

// writeWithRetries retries a publish indefinitely; only context cancellation stops it.
func (s *Service) writeWithRetries(ctx context.Context, writer brokers.Writer, msg brokers.Message) errors.Error {
	err := backoff.Retry(func() error {
		if writeErr := writer.WriteMessages(ctx, msg); writeErr != nil {
			writeErrors.Inc()
			return writeErr
		}
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
		return preparedRecord{kind: terminalDrop}
	}
	return preparedRecord{kind: terminalDLQ, message: msg}
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

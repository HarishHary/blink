package enricher

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
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
)

var (
	alertsIn           = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "alerts_in_total"})
	alertsOut          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "alerts_out_total"})
	alertsDLQ          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "alerts_dlq_total"})
	enrichmentsApplied = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "enrichments_applied_total"}, []string{"enrichment"})
	enrichmentErrors   = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "enrichment_errors_total"}, []string{"enrichment"})
	enrichmentLatency  = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "enrichment_latency_seconds", Buckets: prometheus.DefBuckets}, []string{"enrichment"})
	parseErrors        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "parse_errors_total"})
	readErrors         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "read_errors_total"})
	commitErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "commit_errors_total"})
	writeErrors        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "write_errors_total"})
	concurrencyGauge   = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "alert_enricher", Name: "concurrent_enrichments"})
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

type enrichmentItem struct {
	state    *alertState
	meta     *enrichments.EnrichmentMetadata
	event    events.Event
	err      errors.Error
	attempts int
}

type alertState struct {
	source   brokers.Message
	alert    *alerts.Alert
	items    []*enrichmentItem
	next     int
	failure  *enrichmentItem
	prepared *preparedRecord
}

type enrichmentEntry struct {
	meta  *enrichments.EnrichmentMetadata
	items []*enrichmentItem
}

type batch struct {
	states []*alertState
}

// Service enriches each alert to one forwarded or dead-letter terminal before committing its input.
type Service struct {
	logger          *logger.Logger
	config          Config
	formatterWriter brokers.Writer
	dlqWriter       brokers.Writer
	enrichmentCfg   *enrichments.SnapshotConfig
	pool            *enrichments.Pool
	sem             *semaphore.Weighted
}

// Config contains the environment-loaded settings and runtime dependencies injected by main.
type Config struct {
	Broker         brokers.Broker
	EnricherTopic  string `env:"KAFKA_TOPIC_ENRICHER"`
	EnricherGroup  string `env:"KAFKA_GROUP_ENRICHER"`
	FormatterTopic string `env:"KAFKA_TOPIC_FORMATTER"`
	DLQTopic       string `env:"KAFKA_TOPIC_ENRICHER_DLQ"`
	// ReadyFn gates grouped-consumer creation until the enrichment snapshot catch-up completes.
	ReadyFn func() bool
	// BatchSize is the number of alerts to read from the broker at once.
	BatchSize int `env:"ENRICHER_BATCH_SIZE,optional"`
	// Concurrency is the maximum number of concurrent enrichment pool calls.
	Concurrency int `env:"ENRICHER_CONCURRENCY,optional"`
	// TimeoutSec bounds each enrichment pool call in seconds.
	TimeoutSec int `env:"ENRICHER_TIMEOUT_SEC,optional"`
	// MaxAttempts is how many times a failing enrichment call is tried per alert before DLQ.
	MaxAttempts int `env:"ENRICHER_MAX_ATTEMPTS,optional"`
	// RetryBaseMS is the initial enrichment and publication retry delay in milliseconds.
	RetryBaseMS int `env:"ENRICHER_RETRY_BASE_MS,optional"`
	// RetryCapMS bounds exponential enrichment and publication retry delays in milliseconds.
	RetryCapMS int `env:"ENRICHER_RETRY_CAP_MS,optional"`
}

func NewService(logger *logger.Logger, cfg Config, pool *enrichments.Pool, enrichmentCfg *enrichments.SnapshotConfig) *Service {
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
		logger:          logger,
		config:          cfg,
		formatterWriter: cfg.Broker.NewWriter(cfg.FormatterTopic),
		dlqWriter:       cfg.Broker.NewWriter(cfg.DLQTopic),
		enrichmentCfg:   enrichmentCfg,
		pool:            pool,
		sem:             semaphore.NewWeighted(int64(cfg.Concurrency)),
	}
}

func (s *Service) Name() string { return "alert-enricher" }

func (s *Service) Run(ctx context.Context) errors.Error {
	if !waitForReady(ctx, s.config.ReadyFn) {
		return nil
	}

	s.logger.Info("catalog ready; consuming alerts (topic=%s group=%s)", s.config.EnricherTopic, s.config.EnricherGroup)
	reader := s.config.Broker.NewReader(s.config.EnricherTopic, s.config.EnricherGroup)
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
	s.evaluateEnrichments(ctx, batch.states)
	if ctx.Err() != nil {
		return errors.NewE(ctx.Err())
	}
	if err := s.prepare(batch.states); err != nil {
		return err
	}
	return s.publish(ctx, batch.states)
}

// decode turns every input into ordered alert state and resolves its enrichment chain.
func (s *Service) decode(msgs []brokers.Message) *batch {
	batch := &batch{states: make([]*alertState, len(msgs))}
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
		if alert.Rule == nil {
			parseErrors.Inc()
			prepared := s.prepareDLQ(msg, "decode", "alert has no rule metadata", 0)
			state.prepared = &prepared
			continue
		}
		if alert.Event == nil {
			parseErrors.Inc()
			prepared := s.prepareDLQ(msg, "decode", "alert has no event", 0)
			state.prepared = &prepared
			continue
		}
		alertsIn.Inc()
		state.alert = alert

		metaList, err := enrichmentsForAlert(s.enrichmentCfg, alert.Rule.Enrichments, alert.EnrichmentsApplied)
		if err != nil {
			prepared := s.prepareDLQ(msg, "enrichments", err.Error(), 0)
			state.prepared = &prepared
			continue
		}
		for _, meta := range metaList {
			item := &enrichmentItem{state: state, meta: meta}
			state.items = append(state.items, item)
		}
	}
	return batch
}

// enrichmentsForAlert returns a dependency-first enrichment plan with stable rule order and logical-ID deduplication.
func enrichmentsForAlert(cfg *enrichments.SnapshotConfig, names, appliedNames []string) ([]*enrichments.EnrichmentMetadata, errors.Error) {
	result := make([]*enrichments.EnrichmentMetadata, 0, len(names))
	applied := make(map[string]struct{}, len(appliedNames))
	for _, name := range appliedNames {
		applied[name] = struct{}{}
	}
	seen := make(map[string]struct{})
	visiting := make(map[string]bool)
	var visit func(string) errors.Error
	visit = func(name string) errors.Error {
		if _, ok := applied[name]; ok {
			return nil
		}
		if visiting[name] {
			return errors.NewF("enrichment dependency cycle includes %s", name)
		}
		meta, ok := cfg.ByFileName(name)
		if !ok || !meta.Enabled {
			return errors.NewF("enrichment reference %s is unavailable", name)
		}
		visiting[name] = true
		for _, dependency := range meta.DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[name] = false
		if _, ok := seen[meta.Id]; ok {
			return nil
		}
		seen[meta.Id] = struct{}{}
		result = append(result, meta)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// evaluateEnrichments executes one dependency-ready item per alert in each wave.
func (s *Service) evaluateEnrichments(ctx context.Context, states []*alertState) {
	for {
		entries := nextEnrichmentEntries(states)
		if len(entries) == 0 {
			return
		}

		var wg sync.WaitGroup
		for _, entry := range entries {
			wg.Go(func() {
				s.enrichWithRetries(ctx, entry)
			})
		}
		wg.Wait()
		if ctx.Err() != nil {
			return
		}

		for _, state := range states {
			if state.prepared != nil || state.failure != nil || state.next == len(state.items) {
				continue
			}
			item := state.items[state.next]
			if item.err != nil {
				state.failure = item
				continue
			}
			state.alert.Event = item.event
			state.alert.EnrichmentsApplied = append(state.alert.EnrichmentsApplied, item.meta.Name)
			state.next++
			enrichmentsApplied.WithLabelValues(item.meta.Name).Inc()
		}
	}
}

func nextEnrichmentEntries(states []*alertState) []*enrichmentEntry {
	entries := make([]*enrichmentEntry, 0)
	byEnrichment := make(map[string]*enrichmentEntry)
	for _, state := range states {
		if state.prepared != nil || state.failure != nil || state.next == len(state.items) {
			continue
		}
		item := state.items[state.next]
		entry := byEnrichment[item.meta.Id]
		if entry == nil {
			entry = &enrichmentEntry{meta: item.meta}
			byEnrichment[item.meta.Id] = entry
			entries = append(entries, entry)
		}
		entry.items = append(entry.items, item)
	}
	return entries
}

var errPendingRetries = stderrors.New("enrichment retries pending")

// enrichWithRetries retries only failed alert-enrichment pairs and preserves successful outcomes.
func (s *Service) enrichWithRetries(ctx context.Context, entry *enrichmentEntry) {
	pendingItems := entry.items
	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		pendingAlerts := make([]*alerts.Alert, len(pendingItems))
		for i, item := range pendingItems {
			pendingAlerts[i] = item.state.alert.Clone()
		}

		result := s.enrich(ctx, entry.meta, pendingAlerts)
		if result.CallErr != nil {
			if attempt == s.config.MaxAttempts {
				for _, item := range pendingItems {
					item.err = result.CallErr
					item.attempts = attempt
				}
				return nil
			}
			return errPendingRetries
		}

		next := make([]*enrichmentItem, 0, len(pendingItems))
		for i, item := range pendingItems {
			if result.Errs[i] == nil {
				item.event = pendingAlerts[i].Event
				continue
			}
			if attempt == s.config.MaxAttempts {
				item.err = result.Errs[i]
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

// enrich performs one bounded, timed Pool.Enrich call for the pending alerts.
func (s *Service) enrich(ctx context.Context, meta *enrichments.EnrichmentMetadata, alerts []*alerts.Alert) enrichments.EnrichResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return enrichments.EnrichResult{CallErr: errors.NewE(err)}
	}
	concurrencyGauge.Inc()
	defer func() {
		s.sem.Release(1)
		concurrencyGauge.Dec()
	}()

	enrichCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	result := s.pool.Enrich(enrichCtx, meta.Id, alerts)
	enrichmentLatency.WithLabelValues(meta.Name).Observe(time.Since(start).Seconds())
	if result.CallErr != nil {
		enrichmentErrors.WithLabelValues(meta.Name).Inc()
		return result
	}
	if len(result.Errs) != len(alerts) {
		enrichmentErrors.WithLabelValues(meta.Name).Inc()
		return enrichments.EnrichResult{CallErr: errors.NewF("enrichment %s returned %d errors for %d alerts", meta.Name, len(result.Errs), len(alerts))}
	}
	itemErrors := 0
	for _, err := range result.Errs {
		if err != nil {
			itemErrors++
		}
	}
	if itemErrors > 0 {
		enrichmentErrors.WithLabelValues(meta.Name).Add(float64(itemErrors))
	}
	return result
}

// prepare builds every forwarded alert and dead-letter record before any broker write begins.
func (s *Service) prepare(states []*alertState) errors.Error {
	for _, state := range states {
		if state.prepared != nil {
			continue
		}
		if state.failure != nil {
			reason := fmt.Sprintf("enrichment %s: %s", state.failure.meta.Name, state.failure.err)
			prepared := s.prepareDLQ(state.source, "enrichment", reason, state.failure.attempts)
			state.prepared = &prepared
			continue
		}

		state.alert.EnrichmentsApplied = nil
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
			return errors.New("alert enricher left an input without a terminal state")
		}
		switch state.prepared.kind {
		case terminalDrop:
		case terminalNormal:
			if err := s.writeWithRetries(ctx, s.formatterWriter, state.prepared.message); err != nil {
				return err
			}
			alertsOut.Inc()
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

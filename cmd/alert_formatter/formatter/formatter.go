package formatter

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
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/formatters"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
)

var (
	alertsIn          = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "alerts_in_total"})
	alertsOut         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "alerts_out_total"})
	alertsDLQ         = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "alerts_dlq_total"})
	formattersApplied = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "formatters_applied_total"}, []string{"formatter"})
	formatterErrors   = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "formatter_errors_total"}, []string{"formatter"})
	formatterLatency  = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "formatter_latency_seconds", Buckets: prometheus.DefBuckets}, []string{"formatter"})
	parseErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "parse_errors_total"})
	readErrors        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "read_errors_total"})
	commitErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "commit_errors_total"})
	writeErrors       = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "write_errors_total"})
	concurrencyGauge  = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "alert_formatter", Name: "concurrent_formatters"})
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

type formatterItem struct {
	state    *alertState
	meta     *formatters.FormatterMetadata
	output   map[string]any
	err      errors.Error
	attempts int
}

type alertState struct {
	source   brokers.Message
	alert    *alerts.Alert
	items    []*formatterItem
	next     int
	failure  *formatterItem
	prepared *preparedRecord
}

type formatterEntry struct {
	meta  *formatters.FormatterMetadata
	items []*formatterItem
}

type batch struct {
	states []*alertState
}

// Service formats each alert to one forwarded or dead-letter terminal before committing its input.
type Service struct {
	logger           *logger.Logger
	config           Config
	dispatcherWriter brokers.Writer
	dlqWriter        brokers.Writer
	formatterCfg     *formatters.SnapshotConfig
	pool             *formatters.Pool
	sem              *semaphore.Weighted
}

// Config contains the environment-loaded settings and runtime dependencies injected by main.
type Config struct {
	Broker          brokers.Broker
	FormatterTopic  string `env:"KAFKA_TOPIC_FORMATTER"`
	FormatterGroup  string `env:"KAFKA_GROUP_FORMATTER"`
	DispatcherTopic string `env:"KAFKA_TOPIC_DISPATCHER"`
	DLQTopic        string `env:"KAFKA_TOPIC_FORMATTER_DLQ"`
	// ReadyFn gates grouped-consumer creation until the formatter snapshot catch-up completes.
	ReadyFn func() bool
	// BatchSize is the number of alerts to read from the broker at once.
	BatchSize int `env:"FORMATTER_BATCH_SIZE,optional"`
	// Concurrency is the maximum number of concurrent formatter pool calls.
	Concurrency int `env:"FORMATTER_CONCURRENCY,optional"`
	// TimeoutSec bounds each formatter pool call in seconds.
	TimeoutSec int `env:"FORMATTER_TIMEOUT_SEC,optional"`
	// MaxAttempts is how many times a failing formatter call is tried per alert before DLQ.
	MaxAttempts int `env:"FORMATTER_MAX_ATTEMPTS,optional"`
	// RetryBaseMS is the initial formatter and publication retry delay in milliseconds.
	RetryBaseMS int `env:"FORMATTER_RETRY_BASE_MS,optional"`
	// RetryCapMS bounds exponential formatter and publication retry delays in milliseconds.
	RetryCapMS int `env:"FORMATTER_RETRY_CAP_MS,optional"`
}

func NewService(logger *logger.Logger, cfg Config, pool *formatters.Pool, formatterCfg *formatters.SnapshotConfig) *Service {
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
		logger:           logger,
		config:           cfg,
		dispatcherWriter: cfg.Broker.NewWriter(cfg.DispatcherTopic),
		dlqWriter:        cfg.Broker.NewWriter(cfg.DLQTopic),
		formatterCfg:     formatterCfg,
		pool:             pool,
		sem:              semaphore.NewWeighted(int64(cfg.Concurrency)),
	}
}

func (s *Service) Name() string { return "alert-formatter" }

func (s *Service) Run(ctx context.Context) errors.Error {
	if !waitForReady(ctx, s.config.ReadyFn) {
		return nil
	}

	s.logger.Info("catalog ready; consuming alerts (topic=%s group=%s)", s.config.FormatterTopic, s.config.FormatterGroup)
	reader := s.config.Broker.NewReader(s.config.FormatterTopic, s.config.FormatterGroup)
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
	s.evaluateFormatters(ctx, batch.states)
	if ctx.Err() != nil {
		return errors.NewE(ctx.Err())
	}
	if err := s.prepare(batch.states); err != nil {
		return err
	}
	return s.publish(ctx, batch.states)
}

// decode turns every input into ordered alert state and resolves its formatter chain.
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

		metaList, err := formattersForAlert(s.formatterCfg, alert.Rule.Formatters)
		if err != nil {
			prepared := s.prepareDLQ(msg, "formatters", err.Error(), 0)
			state.prepared = &prepared
			continue
		}
		for _, meta := range metaList {
			item := &formatterItem{state: state, meta: meta}
			state.items = append(state.items, item)
		}
	}
	return batch
}

// formattersForAlert resolves formatter artifacts in declared order and retains repeated entries.
func formattersForAlert(cfg *formatters.SnapshotConfig, names []string) ([]*formatters.FormatterMetadata, errors.Error) {
	result := make([]*formatters.FormatterMetadata, 0, len(names))
	for _, name := range names {
		meta, ok := cfg.ByFileName(name)
		if !ok || !meta.Enabled {
			return nil, errors.NewF("formatter reference %s is unavailable", name)
		}
		result = append(result, meta)
	}
	return result, nil
}

// evaluateFormatters executes one ordered formatter item per alert in each wave.
func (s *Service) evaluateFormatters(ctx context.Context, states []*alertState) {
	for {
		entries := nextFormatterEntries(states)
		if len(entries) == 0 {
			return
		}

		var wg sync.WaitGroup
		for _, entry := range entries {
			wg.Go(func() {
				s.formatWithRetries(ctx, entry)
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
			maps.Copy(state.alert.Event, item.output)
			state.next++
			formattersApplied.WithLabelValues(item.meta.Name).Inc()
		}
	}
}

func nextFormatterEntries(states []*alertState) []*formatterEntry {
	entries := make([]*formatterEntry, 0)
	byFormatter := make(map[string]*formatterEntry)
	for _, state := range states {
		if state.prepared != nil || state.failure != nil || state.next == len(state.items) {
			continue
		}
		item := state.items[state.next]
		entry := byFormatter[item.meta.Id]
		if entry == nil {
			entry = &formatterEntry{meta: item.meta}
			byFormatter[item.meta.Id] = entry
			entries = append(entries, entry)
		}
		entry.items = append(entry.items, item)
	}
	return entries
}

var errPendingRetries = stderrors.New("formatter retries pending")

// formatWithRetries retries only failed alert-formatter pairs and preserves successful outcomes.
func (s *Service) formatWithRetries(ctx context.Context, entry *formatterEntry) {
	pendingItems := entry.items
	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		pendingAlerts := make([]*alerts.Alert, len(pendingItems))
		for i, item := range pendingItems {
			pendingAlerts[i] = item.state.alert.Clone()
		}

		result := s.format(ctx, entry.meta, pendingAlerts)
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

		next := make([]*formatterItem, 0, len(pendingItems))
		for i, item := range pendingItems {
			if result.Items[i].Err == nil {
				item.output = result.Items[i].Output
				continue
			}
			if attempt == s.config.MaxAttempts {
				item.err = result.Items[i].Err
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

// format performs one bounded, timed Pool.Format call for the pending alerts.
func (s *Service) format(ctx context.Context, meta *formatters.FormatterMetadata, alerts []*alerts.Alert) formatters.FormatResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return formatters.FormatResult{CallErr: errors.NewE(err)}
	}
	concurrencyGauge.Inc()
	defer func() {
		s.sem.Release(1)
		concurrencyGauge.Dec()
	}()

	formatCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	result := s.pool.Format(formatCtx, meta.Id, alerts)
	formatterLatency.WithLabelValues(meta.Name).Observe(time.Since(start).Seconds())
	if result.CallErr != nil {
		formatterErrors.WithLabelValues(meta.Name).Inc()
		return result
	}
	if len(result.Items) != len(alerts) {
		formatterErrors.WithLabelValues(meta.Name).Inc()
		return formatters.FormatResult{CallErr: errors.NewF("formatter %s returned %d items for %d alerts", meta.Name, len(result.Items), len(alerts))}
	}
	itemErrors := 0
	for _, item := range result.Items {
		if item.Err != nil {
			itemErrors++
		}
	}
	if itemErrors > 0 {
		formatterErrors.WithLabelValues(meta.Name).Add(float64(itemErrors))
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
			reason := fmt.Sprintf("formatter %s: %s", state.failure.meta.Name, state.failure.err)
			prepared := s.prepareDLQ(state.source, "formatter", reason, state.failure.attempts)
			state.prepared = &prepared
			continue
		}

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
			return errors.New("alert formatter left an input without a terminal state")
		}
		switch state.prepared.kind {
		case terminalDrop:
		case terminalNormal:
			if err := s.writeWithRetries(ctx, s.dispatcherWriter, state.prepared.message); err != nil {
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

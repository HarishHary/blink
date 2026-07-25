package executor

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq"
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

	dlqOut         = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "dlq_records_total"}, []string{"stage"})
	dlqWriteErrors = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "dlq_write_errors_total"})

	ruleMatches = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "rule_executor", Name: "rule_matches_total"}, []string{"rule"})
)

type ruleEntry struct {
	meta   *rules.RuleMetadata
	events []events.Event
	// sources[i] is the Kafka record events[i] was decoded from, kept so an evaluation
	// failure can be dead-lettered against its origin instead of killing the batch.
	sources  []brokers.Message
	results  []rules.EvaluateItem
	failure  string // non-empty once evaluation is exhausted; sources go to the DLQ
	attempts int
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

// Config is the explicit set of dependencies NewService needs. The composition
// root (cmd/rule_executor/main) loads config once and injects these, rather than the
// component reaching into a shared ServiceContext / re-loading the whole environment.
// Config's topic fields (and the optional EXECUTOR_* tuning knobs) are populated from the
// environment by main (which embeds it); Broker and Ready are injected after load.
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

// batch is one poll's decoded work: the rules to evaluate, in first-seen order, plus the
// dead-letter records collected along the way.
type batch struct {
	byRule    map[string]*ruleEntry
	ruleOrder []*ruleEntry
	dlq       []brokers.Message
}

// route appends an event to its rule's work, registering the rule on first sight.
func (b *batch) route(meta *rules.RuleMetadata, event events.Event, source brokers.Message) {
	e, exists := b.byRule[meta.Id]
	if !exists {
		e = &ruleEntry{meta: meta}
		b.byRule[meta.Id] = e
		b.ruleOrder = append(b.ruleOrder, e)
	}
	e.events = append(e.events, event)
	e.sources = append(e.sources, source)
}

// processBatch decodes messages, groups events by rule, evaluates every rule, then
// publishes each rule's matched events in original input order.
func (s *Service) processBatch(ctx context.Context, msgs []brokers.Message, ruleSet []*rules.RuleMetadata) errors.Error {
	b := s.decode(msgs, ruleSet)
	if len(b.ruleOrder) > 0 {
		rulesPerBatch.Observe(float64(len(b.ruleOrder)))
		s.logger.Info("evaluating %d rule(s) across batch of %d message(s)", len(b.ruleOrder), len(msgs))
		s.evaluate(ctx, b.ruleOrder)
		if ctx.Err() != nil {
			return nil // shutdown: leave the batch uncommitted for redelivery
		}
	}
	return s.publish(ctx, b)
}

// decode turns raw records into per-rule work. Input faults are per-record and never fail the
// batch: an uncommitted poison pill would be redelivered forever.
func (s *Service) decode(msgs []brokers.Message, ruleSet []*rules.RuleMetadata) *batch {
	b := &batch{byRule: make(map[string]*ruleEntry)}
	for _, m := range msgs {
		var msg execpb.ExecMessage
		if err := proto.Unmarshal(m.Value, &msg); err != nil {
			eventsParseErrors.Inc()
			s.deadLetter(b, m, "decode", err.Error(), 0)
			continue
		}
		if msg.GetEvent() == nil {
			eventsParseErrors.Inc()
			s.deadLetter(b, m, "decode", "exec message has no event", 0)
			continue
		}

		event := msg.GetEvent().AsMap()
		lt, ok := event["log_type"].(string)
		if !ok {
			eventsInvalidLogType.Inc()
			s.deadLetter(b, m, "log_type", "event log_type must be a string", 0)
			continue
		}

		metaList, err := s.eligibleRules(ruleSet, lt, msg.GetRuleIds())
		if err != nil {
			s.deadLetter(b, m, "rules", err.Error(), 0)
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
			b.route(meta, event, m)
		}
	}
	return b
}

// evaluate runs every rule's pool call in parallel, bounded by EXECUTOR_CONCURRENCY. Evaluation
// failures are recorded on the entry rather than returned, so nothing here fails the batch.
func (s *Service) evaluate(ctx context.Context, entries []*ruleEntry) {
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Acquire only fails once ctx is done; the caller re-checks ctx and leaves
			// the batch uncommitted, so a half-evaluated entry is never published.
			if err := s.sem.Acquire(ctx, 1); err != nil {
				return
			}
			concurrencyGauge.Inc()
			defer func() {
				s.sem.Release(1)
				concurrencyGauge.Dec()
			}()
			s.evaluateWithRetries(ctx, entry)
		}()
	}
	wg.Wait()
}

// publish writes each rule's alerts, dead-letters the rules whose evaluation was exhausted,
// then flushes every dead-letter record before the caller commits the batch's offsets.
func (s *Service) publish(ctx context.Context, b *batch) errors.Error {
	for _, entry := range b.ruleOrder {
		if entry.failure != "" {
			// ponytail: an event routed to N failing rules yields N DLQ records, one per
			// rule. Dedupe by (partition, offset) only if replay volume proves it matters.
			reason := fmt.Sprintf("rule %s: %s", entry.meta.Name, entry.failure)
			for _, src := range entry.sources {
				s.deadLetter(b, src, "rule", reason, entry.attempts)
			}
			continue
		}
		if err := s.publishRuleResults(ctx, entry); err != nil {
			return err
		}
	}
	for _, rec := range b.dlq {
		if err := s.writeWithRetries(ctx, s.dlqWriter, rec, dlqWriteErrors); err != nil {
			return err
		}
	}
	return nil
}

// evaluateWithRetries retries the rule's pool call up to MaxAttempts, each attempt bounded by
// TimeoutSec. On exhaustion it records the failure on the entry so the caller dead-letters that
// rule's source records, rather than failing the batch and redelivering it indefinitely.
func (s *Service) evaluateWithRetries(ctx context.Context, entry *ruleEntry) {
	attempt := 0
	err := backoff.Retry(func() error {
		attempt++
		cctx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
		defer cancel()
		results, evalErr := s.evaluateRule(cctx, entry.meta, entry.events)
		if evalErr != nil {
			if attempt >= s.config.MaxAttempts {
				return backoff.Permanent(evalErr)
			}
			return evalErr
		}
		entry.results = results
		return nil
	}, s.newBackoff(ctx))
	if err != nil {
		entry.attempts = attempt
		entry.failure = err.Error()
	}
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

func (s *Service) publishRuleResults(ctx context.Context, entry *ruleEntry) errors.Error {
	for i, result := range entry.results {
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
		if err := s.writeWithRetries(ctx, s.mergerWriter, brokers.Message{
			Key:   []byte(alert.MergePartitionKey()),
			Value: payload,
		}, alertsWriteErrors); err != nil {
			return err
		}
		alertsWriteDuration.Observe(time.Since(startWrite).Seconds())
		ruleMatches.WithLabelValues(entry.meta.Name).Inc()
		alertsOut.Inc()
	}
	return nil
}

// writeWithRetries retries a publish indefinitely; only ctx cancellation stops it. A dropped
// write would silently lose an alert (or the evidence of a failure), so there is no give-up path.
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

// deadLetter queues one record for the dead-letter topic. A marshal failure is unreachable for a
// well-formed envelope, so it is logged and dropped: failing the batch instead would leave the
// offending record uncommitted and replay it forever.
func (s *Service) deadLetter(b *batch, source brokers.Message, stage, reason string, attempts int) {
	msg, err := dlq.Record(source, stage, reason, attempts)
	if err != nil {
		s.logger.ErrorF("dropping dead-letter record (stage=%s): %v", stage, err)
		return
	}
	dlqOut.WithLabelValues(stage).Inc()
	b.dlq = append(b.dlq, msg)
}

// newBackoff returns the service's exponential retry policy (RetryBaseMS initial, RetryCapMS cap, jittered).
func (s *Service) newBackoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = time.Duration(s.config.RetryBaseMS) * time.Millisecond
	b.MaxInterval = time.Duration(s.config.RetryCapMS) * time.Millisecond
	b.MaxElapsedTime = 0 // callers bound attempts themselves or retry until ctx cancel
	return backoff.WithContext(b, ctx)
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

package matcher

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq"
	"github.com/harishhary/blink/internal/errors"
	execpb "github.com/harishhary/blink/internal/exec/pb"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

var (
	eventsIn        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "events_in_total"})
	eventsForwarded = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "events_forwarded_total"})
	readErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "read_errors_total"})
	parseErrors     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "parse_errors_total"})
	writeErrors     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "write_errors_total"})
	matchDuration   = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "match_duration_seconds", Buckets: prometheus.DefBuckets}, []string{"matcher"})
	rulesRouted     = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "rules_routed_per_event", Buckets: []float64{0, 1, 5, 10, 25, 50, 100}})
)

// Service resolves each fetched event to a drop, executor record, or DLQ record before publishing in input order.
type Service struct {
	logger         *logger.Logger
	config         Config
	executorWriter brokers.Writer
	dlqWriter      brokers.Writer
	ruleCfg        *rules.SnapshotConfig    // rollout-authoritative rule snapshot
	matcherCfg     *matchers.SnapshotConfig // resolves matcher file names to metadata
	pool           *matchers.Pool
	sem            *semaphore.Weighted // bounds concurrent Pool.Match calls
}

// Config is the set of dependencies NewService needs, injected by main. See docs/services/event_matcher.md.
type Config struct {
	Broker        brokers.Broker
	MatcherTopic  string `env:"KAFKA_TOPIC_MATCHER"`
	MatcherGroup  string `env:"KAFKA_GROUP_MATCHER"`
	ExecutorTopic string `env:"KAFKA_TOPIC_EXECUTOR"`
	DLQTopic      string `env:"KAFKA_TOPIC_MATCHER_DLQ"`
	// ReadyFn gates grouped-consumer creation until matcher and rule snapshots catch up.
	ReadyFn func() bool
	// BatchSize is the number of events to process in a single batch.
	BatchSize int `env:"MATCHER_BATCH_SIZE,optional"`
	// Concurrency is the maximum number of concurrent matcher pool calls.
	Concurrency int `env:"MATCHER_CONCURRENCY,optional"`
	// TimeoutSec bounds each matcher pool call in seconds.
	TimeoutSec int `env:"MATCHER_TIMEOUT_SEC,optional"`
	// MaxAttempts is how many times a failing matcher call is tried per event before DLQ.
	MaxAttempts int `env:"MATCHER_MAX_ATTEMPTS,optional"`
	// RetryBaseMS is the initial matcher and publication retry delay in milliseconds.
	RetryBaseMS int `env:"MATCHER_RETRY_BASE_MS,optional"`
	// RetryCapMS bounds exponential matcher and publication retry delays in milliseconds.
	RetryCapMS int `env:"MATCHER_RETRY_CAP_MS,optional"`
}

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

type ruleCandidate struct {
	meta     *rules.RuleMetadata
	eligible bool
}

type matcherFailure struct {
	matcher  string
	reason   string
	attempts int
}

type eventState struct {
	mu         sync.Mutex
	event      events.Event
	source     brokers.Message
	candidates []*ruleCandidate
	failure    *matcherFailure
	prepared   *preparedRecord
}

type matcherItem struct {
	state      *eventState
	candidates []*ruleCandidate
}

type matcherEntry struct {
	meta  *matchers.MatcherMetadata
	items []matcherItem
}

func NewService(logger *logger.Logger, cfg Config, pool *matchers.Pool, ruleCfg *rules.SnapshotConfig, matcherCfg *matchers.SnapshotConfig) *Service {
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
		executorWriter: cfg.Broker.NewWriter(cfg.ExecutorTopic),
		dlqWriter:      cfg.Broker.NewWriter(cfg.DLQTopic),
		ruleCfg:        ruleCfg,
		matcherCfg:     matcherCfg,
		pool:           pool,
		sem:            semaphore.NewWeighted(int64(cfg.Concurrency)),
	}
}

func (s *Service) Name() string { return "event-matcher" }

func (s *Service) Run(ctx context.Context) errors.Error {
	if !waitForReady(ctx, s.config.ReadyFn) {
		return nil
	}

	s.logger.Info("catalogs ready; consuming events (topic=%s group=%s)", s.config.MatcherTopic, s.config.MatcherGroup)
	reader := s.config.Broker.NewReader(s.config.MatcherTopic, s.config.MatcherGroup)
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
	states := s.decode(msgs)
	s.evaluateMatchers(ctx, states)
	// Cancellation leaves unresolved states uncommitted for redelivery.
	if ctx.Err() != nil {
		return errors.NewE(ctx.Err())
	}
	if err := s.prepare(states); err != nil {
		return err
	}
	return s.publish(ctx, states)
}

// decode turns every input into ordered event state; invalid records become prepared DLQ records.
func (s *Service) decode(msgs []brokers.Message) []*eventState {
	ruleSet := s.ruleCfg.Primaries()
	states := make([]*eventState, len(msgs))
	for i, msg := range msgs {
		state := &eventState{source: msg}
		states[i] = state
		var evt events.Event
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			parseErrors.Inc()
			prepared := s.prepareDLQ(msg, "decode", err.Error(), 0)
			state.prepared = &prepared
			continue
		}
		eventsIn.Inc()

		logType, ok := evt["log_type"].(string)
		if !ok {
			prepared := s.prepareDLQ(msg, "log_type", "event log_type must be a string", 0)
			state.prepared = &prepared
			continue
		}

		candidates := rules.RulesForLogTypeIn(ruleSet, logType)
		if len(candidates) == 0 {
			state.prepared = &preparedRecord{kind: terminalDrop}
			continue
		}

		state.event = evt
		state.candidates = make([]*ruleCandidate, len(candidates))
		for i, candidate := range candidates {
			state.candidates[i] = &ruleCandidate{meta: candidate, eligible: true}
		}
	}
	return states
}

// groupByMatcher groups each event's rule candidates by required matcher.
func (s *Service) groupByMatcher(states []*eventState) map[string]*matcherEntry {
	byMatcher := make(map[string]*matcherEntry)
	for _, state := range states {
		if state.prepared != nil {
			continue
		}
		perMatcher := make(map[string][]*ruleCandidate)
		metadata := make(map[string]*matchers.MatcherMetadata)
		for _, candidate := range state.candidates {
			for _, name := range candidate.meta.Matchers {
				matcher, ok := s.matcherCfg.ByFileName(name)
				if !ok || !matcher.Enabled {
					state.recordFailure(name, "matcher reference is unavailable", 0)
					continue
				}
				perMatcher[matcher.Id] = append(perMatcher[matcher.Id], candidate)
				metadata[matcher.Id] = matcher
			}
		}
		if state.failure != nil {
			continue
		}
		for id, candidates := range perMatcher {
			entry := byMatcher[id]
			if entry == nil {
				entry = &matcherEntry{meta: metadata[id]}
				byMatcher[id] = entry
			}
			entry.items = append(entry.items, matcherItem{state: state, candidates: candidates})
		}
	}
	return byMatcher
}

func (s *eventState) recordFailure(matcher, reason string, attempts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil || matcher < s.failure.matcher {
		s.failure = &matcherFailure{matcher: matcher, reason: reason, attempts: attempts}
	}
}

func (s *Service) evaluateMatchers(ctx context.Context, states []*eventState) {
	var wg sync.WaitGroup
	for _, entry := range s.groupByMatcher(states) {
		wg.Go(func() {
			s.matchWithRetries(ctx, entry)
		})
	}
	wg.Wait()
}

// errPendingRetries drives the backoff between attempts; outcomes live in eventState, not the error.
var errPendingRetries = stderrors.New("matcher retries pending")

// matchWithRetries owns one matcher's failed subset across attempts.
func (s *Service) matchWithRetries(ctx context.Context, entry *matcherEntry) {
	pendingItems := entry.items
	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		pendingEvents := make([]events.Event, len(pendingItems))
		for i, item := range pendingItems {
			pendingEvents[i] = item.state.event
		}
		result := s.match(ctx, entry.meta, pendingEvents)
		if ctx.Err() != nil {
			// Cancellation leaves the batch uncommitted.
			return backoff.Permanent(errPendingRetries)
		}

		if result.CallErr != nil {
			if attempt == s.config.MaxAttempts {
				for _, item := range pendingItems {
					s.logger.Error(result.CallErr)
					item.state.recordFailure(entry.meta.Id, fmt.Sprint(result.CallErr.Message()), attempt)
				}
				return nil
			}
			return errPendingRetries
		}

		next := make([]matcherItem, 0, len(pendingItems))
		for i, item := range pendingItems {
			match := result.Items[i]
			if match.Err != nil {
				s.logger.Error(match.Err)
				if attempt == s.config.MaxAttempts {
					item.state.recordFailure(entry.meta.Id, fmt.Sprint(match.Err.Message()), attempt)
				} else {
					next = append(next, item)
				}
				continue
			}
			if !match.Matched {
				item.state.mu.Lock()
				for _, candidate := range item.candidates {
					candidate.eligible = false
				}
				item.state.mu.Unlock()
			}
		}
		pendingItems = next
		if len(pendingItems) == 0 {
			return nil
		}
		return errPendingRetries
	}, s.newBackoff(ctx))
}

// match performs one bounded, timed Pool.Match call for the pending items.
func (s *Service) match(ctx context.Context, matcher *matchers.MatcherMetadata, events []events.Event) matchers.MatchResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return matchers.MatchResult{CallErr: errors.NewE(err)}
	}
	defer s.sem.Release(1)

	matchCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	result := s.pool.Match(matchCtx, matcher.Id, events)
	matchDuration.WithLabelValues(matcher.Name).Observe(time.Since(start).Seconds())
	if result.CallErr == nil && len(result.Items) != len(events) {
		return matchers.MatchResult{CallErr: errors.NewF("matcher %s returned invalid result shape", matcher.Name)}
	}
	return result
}

// prepare builds every terminal record before any broker write begins.
func (s *Service) prepare(states []*eventState) errors.Error {
	for _, state := range states {
		if state.prepared != nil {
			continue
		}
		if state.failure != nil {
			failure := state.failure
			prepared := s.prepareDLQ(state.source, "matcher", fmt.Sprintf("matcher %s: %s", failure.matcher, failure.reason), failure.attempts)
			state.prepared = &prepared
			continue
		}

		var ruleIDs []string
		for _, candidate := range state.candidates {
			if candidate.eligible {
				ruleIDs = append(ruleIDs, candidate.meta.Id)
			}
		}
		rulesRouted.Observe(float64(len(ruleIDs)))

		if len(ruleIDs) == 0 {
			state.prepared = &preparedRecord{kind: terminalDrop}
			continue
		}

		eventStruct, err := structpb.NewStruct(state.event)
		if err != nil {
			parseErrors.Inc()
			return errors.NewE(err)
		}
		payload, err := proto.Marshal(&execpb.ExecMessage{
			Event:   eventStruct,
			RuleIds: ruleIDs,
		})
		if err != nil {
			return errors.NewE(err)
		}
		state.prepared = &preparedRecord{kind: terminalNormal, message: brokers.Message{
			Key: append([]byte(nil), state.source.Key...), Value: payload,
		}}
	}
	return nil
}

// publish writes prepared records serially in fetched order, retrying indefinitely.
func (s *Service) publish(ctx context.Context, states []*eventState) errors.Error {
	for _, state := range states {
		if state.prepared == nil {
			return errors.NewF("event matcher left an input without a terminal state")
		}
		if state.prepared.kind == terminalDrop {
			continue
		}
		writer := s.executorWriter
		if state.prepared.kind == terminalDLQ {
			writer = s.dlqWriter
		}
		if err := s.writeWithRetries(ctx, writer, state.prepared.message, writeErrors); err != nil {
			return err
		}
		if state.prepared.kind == terminalNormal {
			eventsForwarded.Inc()
		}
	}
	return nil
}

// writeWithRetries retries a publish indefinitely; only ctx cancellation stops it.
func (s *Service) writeWithRetries(ctx context.Context, w brokers.Writer, msg brokers.Message, errCount prometheus.Counter) errors.Error {
	err := backoff.Retry(func() error {
		if writeErr := w.WriteMessages(ctx, msg); writeErr != nil {
			errCount.Inc()
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

// newBackoff returns the service's exponential retry policy (RetryBaseMS initial, RetryCapMS cap, jittered).
func (s *Service) newBackoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(time.Duration(s.config.RetryBaseMS)*time.Millisecond),
		backoff.WithMaxInterval(time.Duration(s.config.RetryCapMS)*time.Millisecond),
		backoff.WithMaxElapsedTime(0),
	)
	return backoff.WithContext(b, ctx)
}

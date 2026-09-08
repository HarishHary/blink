package matcher

import (
	"context"
	"encoding/json"
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
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/encoding/protowire"
)

var (
	durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	batchBuckets    = []float64{0, 1, 10, 50, 100, 500, 1000, 5000, 10000, 50000}
	eventsIn        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "events_in_total", Help: "Records returned by successful broker batch reads, including invalid records."})
	batchSize       = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "batch_size", Help: "Records returned by each successful broker batch read.", Buckets: batchBuckets})
	readBatchTotal  = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "read_batch_total", Help: "Broker batch read attempts by result."}, []string{"result"})
	readBatchTime   = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "read_batch_seconds", Help: "Duration of broker batch read attempts.", Buckets: durationBuckets})
	batchTotal      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "batch_processing_total", Help: "Fetched batch resolution attempts by result, excluding broker reads and commits."}, []string{"result"})
	batchTime       = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "batch_processing_seconds", Help: "Duration of fetched batch resolution, excluding broker reads and commits.", Buckets: durationBuckets})
	commitTotal     = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "commit_total", Help: "Broker commit attempts by result."}, []string{"result"})
	commitTime      = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "commit_seconds", Help: "Duration of broker commit attempts.", Buckets: durationBuckets})
	evaluationTotal = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "evaluation_total", Help: "Admitted matcher runtime call attempts by plugin and result."}, []string{"plugin", "result"})
	evaluationTime  = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "evaluation_seconds", Help: "Matcher runtime call duration after admission, excluding retry backoff.", Buckets: durationBuckets}, []string{"plugin"})
	evaluationItems = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "evaluation_items_total", Help: "Matcher item attempts by plugin and result."}, []string{"plugin", "result"})
	evaluationRetry = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "evaluation_retries_total", Help: "Additional admitted matcher runtime calls after the first call in a retry loop."}, []string{"plugin"})
	evaluationsLive = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "evaluations_in_flight", Help: "Matcher runtime calls currently admitted."})
	writeTotal      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "write_total", Help: "Broker write attempts by destination and result."}, []string{"destination", "result"})
	writeTime       = promauto.NewHistogramVec(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "write_seconds", Help: "Duration of broker write attempts by destination.", Buckets: durationBuckets}, []string{"destination"})
	writeRetry      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "write_retries_total", Help: "Additional broker write attempts after the first call in a retry loop."}, []string{"destination"})
	recordsOut      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "records_out_total", Help: "Records acknowledged by broker writes by destination."}, []string{"destination"})
	dlqRecords      = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "dlq_records_total", Help: "Dead-letter records acknowledged by broker writes by processing stage."}, []string{"stage"})
	drops           = promauto.NewCounterVec(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "drops_total", Help: "Drop decisions by scope and reason; may include decisions from batches that are not committed."}, []string{"scope", "reason"})
	rulesRouted     = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "rules_routed_per_event", Help: "Eligible rules routed for each valid source event.", Buckets: []float64{0, 1, 5, 10, 25, 50, 100}})
	batchReplays    = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "batch_replays_total", Help: "Additional fetched batch resolution attempts after matcher generation changes."})
)

const (
	readinessInterval = 500 * time.Millisecond
	readinessTimeout  = time.Second
	readinessGrace    = 2 * time.Second
	stateWaitMargin   = time.Second
)

// MatcherRuntime is the matcher call surface consumed by Service.
type MatcherRuntime interface {
	Match(context.Context, snapshot.ProjectionState[*matchers.MatcherMetadata], string, *events.Batch) matchers.MatchResult
	State(context.Context) (snapshot.ProjectionState[*matchers.MatcherMetadata], error)
	Status(context.Context) (plugin.SupervisorStatus, error)
}

// RuleStateSource supplies committed rule metadata for routing, not rule execution.
type RuleStateSource interface {
	State(context.Context) (snapshot.ProjectionState[*rules.RuleMetadata], error)
}

// Config supplies service dependencies and settings; see docs/services/event_matcher.md.
type Config struct {
	Broker             brokers.Broker
	MatcherTopic       string `env:"KAFKA_TOPIC_MATCHER"`
	MatcherGroup       string `env:"KAFKA_GROUP_MATCHER"`
	ExecutorTopic      string `env:"KAFKA_TOPIC_EXECUTOR"`
	DLQTopic           string `env:"KAFKA_TOPIC_MATCHER_DLQ"`
	MaxBatchSize       int    `env:"MAX_BATCH_SIZE,optional"`
	MaxConcurrentCalls int    `env:"MAX_CONCURRENT_CALLS,optional"`
	TimeoutSec         int    `env:"TIMEOUT_SEC,optional"`
	MaxAttempts        int    `env:"MAX_ATTEMPTS,optional"`
	RetryBaseMS        int    `env:"RETRY_BASE_MS,optional"`
	RetryCapMS         int    `env:"RETRY_CAP_MS,optional"`
}

// Service resolves each fetched event to a drop, executor record, or DLQ record before publishing in input order.
type Service struct {
	logger               *logger.Logger
	config               Config
	matcherRuntime       MatcherRuntime
	ruleState            RuleStateSource
	executorWriter       brokers.Writer
	dlqWriter            brokers.Writer
	sem                  *semaphore.Weighted
	ready                atomic.Bool
	snapshotsReady       atomic.Bool
	readyAt              atomic.Int64
	lastRuleAvailability runtime.Availability
}

// batch pins snapshots so a rollover cannot change in-flight routing.
type batch struct {
	msgs         []brokers.Message
	matcherState snapshot.ProjectionState[*matchers.MatcherMetadata]
	ruleState    snapshot.ProjectionState[*rules.RuleMetadata]
	states       []*eventState
}

// terminalKind is the disposition an input reaches: dropped, forwarded, or dead-lettered.
type terminalKind uint8

const (
	terminalDrop terminalKind = iota
	terminalNormal
	terminalDLQ
)

// preparedRecord is one input's terminal disposition and the message it publishes, if any.
type preparedRecord struct {
	kind    terminalKind
	message brokers.Message
	stage   string
}

// ruleCandidate is a rule selected by log type, still eligible until a matcher rejects it.
type ruleCandidate struct {
	meta     *rules.RuleMetadata
	eligible bool
}

// matcherFailure is the reason one event could not be matched, carried into its DLQ record.
type matcherFailure struct {
	matcher  string
	reason   string
	attempts int
}

// eventState is all per-input state for one batch position; its mutex guards the concurrent matcher groups.
type eventState struct {
	mu         sync.Mutex
	event      events.Event
	raw        []byte
	source     brokers.Message
	candidates []*ruleCandidate
	failure    *matcherFailure
	prepared   *preparedRecord
}

// matcherItem tracks an event's candidates and latest failure for one matcher.
type matcherItem struct {
	state      *eventState
	candidates []*ruleCandidate
	reason     string
}

// matcherEntry is every event in the batch that needs the same matcher, evaluated in one call.
type matcherEntry struct {
	meta  *matchers.MatcherMetadata
	items []matcherItem
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

func NewService(logger *logger.Logger, cfg Config, matcherRuntime MatcherRuntime, ruleState RuleStateSource) *Service {
	cfg = cfg.WithDefaults()

	return &Service{
		logger:         logger,
		config:         cfg,
		matcherRuntime: matcherRuntime,
		ruleState:      ruleState,
		executorWriter: cfg.Broker.NewWriter(cfg.ExecutorTopic),
		dlqWriter:      cfg.Broker.NewWriter(cfg.DLQTopic),
		sem:            semaphore.NewWeighted(int64(cfg.MaxConcurrentCalls)),
	}
}

// Name identifies the service to the Runner.
func (s *Service) Name() string { return "event-matcher" }

// Ready reports cached readiness without issuing projection calls for each probe.
func (s *Service) Ready() bool { return s.ready.Load() && s.snapshotsReady.Load() }

// Run consumes batches until failure or cancellation, leaving unfinished batches uncommitted.
func (s *Service) Run(ctx context.Context) errors.Error {
	s.ready.Store(false)
	s.snapshotsReady.Store(false)
	s.readyAt.Store(0)
	defer s.ready.Store(false)

	// Wait for the poller to stop so an in-flight refresh can't publish a verdict after Run returns.
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
	// Readiness allows empty snapshots; consumption requires the stricter gate below.
	s.ready.Store(true)

	if err := s.waitForReady(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.NewE(err)
	}
	// Seed readiness before the first matcher call can temporarily block state reads.
	s.readyAt.Store(time.Now().UnixNano())
	s.snapshotsReady.Store(true)

	s.logger.Info("runtime ready; consuming events (topic=%s group=%s)", s.config.MatcherTopic, s.config.MatcherGroup)
	reader := s.config.Broker.NewReader(s.config.MatcherTopic, s.config.MatcherGroup)
	defer func() {
		if err := reader.Close(); err != nil && ctx.Err() == nil {
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
			return errors.NewE(err)
		}
		eventsIn.Add(float64(len(msgs)))
		batchSize.Observe(float64(len(msgs)))

		start = time.Now()
		batchErr := s.resolveBatch(ctx, msgs)
		batchTime.Observe(time.Since(start).Seconds())
		batchTotal.WithLabelValues(metricResult(ctx, batchErr)).Inc()
		if batchErr != nil {
			if ctx.Err() != nil {
				return nil
			}
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
			return errors.NewE(commitErr)
		}
	}
}

// waitForReady requires nonempty ready projections and a routable matcher runtime before consumption.
func (s *Service) waitForReady(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		matcherState, matcherErr := s.matcherRuntime.State(ctx)
		ruleState, ruleErr := s.ruleState.State(ctx)
		matcherStatus, matcherStatusErr := s.matcherRuntime.Status(ctx)
		if matcherErr == nil && ruleErr == nil && matcherStatusErr == nil &&
			matcherState.Availability == runtime.AvailabilityReady && len(matcherState.Primaries) > 0 &&
			ruleState.Availability == runtime.AvailabilityReady && len(ruleState.Primaries) > 0 &&
			matcherStatus.Availability == runtime.AvailabilityReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollReadiness refreshes readiness, allowing a grace period for degraded or unreadable projections.
func (s *Service) pollReadiness(ctx context.Context) {
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()
	for {
		callCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
		matcherState, matcherErr := s.matcherRuntime.State(callCtx)
		ruleState, ruleErr := s.ruleState.State(callCtx)
		cancel()
		if matcherErr == nil && ruleErr == nil &&
			matcherState.Availability == runtime.AvailabilityReady &&
			ruleState.Availability == runtime.AvailabilityReady {
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

// errBatchReplay signals a generation change before the batch was published.
var errBatchReplay = stderrors.New("matcher generation changed mid-batch")

// resolveBatch retries unpublished batches after generation changes, up to MaxAttempts.
func (s *Service) resolveBatch(ctx context.Context, msgs []brokers.Message) errors.Error {
	policy := s.newBackoff(ctx)
	for attempt := 1; ; attempt++ {
		err := s.processBatch(ctx, msgs)
		if err == nil || !stderrors.Is(err, errBatchReplay) || attempt >= s.config.MaxAttempts {
			return err
		}
		s.logger.Info("matcher generation changed mid-batch; replaying %d records (attempt %d)", len(msgs), attempt)
		if !wait(ctx, policy) {
			return err
		}
		batchReplays.Inc()
	}
}

// processBatch resolves one fetched batch to terminals and publishes them.
func (s *Service) processBatch(ctx context.Context, msgs []brokers.Message) errors.Error {
	b, err := s.newBatch(ctx, msgs)
	if err != nil {
		return errors.NewE(err)
	}
	s.decode(b)
	unavailable := s.evaluateMatchers(ctx, b)
	// Cancellation leaves unresolved states uncommitted for redelivery.
	if ctx.Err() != nil {
		return errors.NewE(ctx.Err())
	}
	// Only a generation change invalidates the batch; other unavailable errors keep their dead-letters.
	if unavailable {
		if state, err := s.readMatcherState(ctx); err == nil && state.CommittedGeneration != b.matcherState.CommittedGeneration {
			return errors.NewE(errBatchReplay)
		}
	}
	s.prepare(b)
	return s.publish(ctx, b)
}

// newBatch pins snapshots, allowing degraded rules but rejecting unavailable rules to avoid silent drops.
func (s *Service) newBatch(ctx context.Context, msgs []brokers.Message) (*batch, error) {
	matcherState, err := s.readMatcherState(ctx)
	if err != nil {
		return nil, err
	}
	ruleState, err := s.ruleState.State(ctx)
	if err != nil {
		return nil, err
	}
	if !ruleState.Availability.Routable() {
		return nil, runtime.ErrSnapshotRead
	}
	if previous := s.lastRuleAvailability; previous != ruleState.Availability {
		s.lastRuleAvailability = ruleState.Availability
		if ruleState.Availability != runtime.AvailabilityReady {
			s.logger.ErrorF("rule snapshot %s; routing on last committed generation %d", ruleState.Availability, ruleState.CommittedGeneration)
		} else if previous != "" {
			s.logger.Info("rule snapshot recovered; routing on generation %d", ruleState.CommittedGeneration)
		}
	}
	return &batch{msgs: msgs, matcherState: matcherState, ruleState: ruleState}, nil
}

// readMatcherState retries unavailable state reads for the matcher timeout plus stateWaitMargin.
func (s *Service) readMatcherState(ctx context.Context) (snapshot.ProjectionState[*matchers.MatcherMetadata], error) {
	deadline := time.Now().Add(time.Duration(s.config.TimeoutSec)*time.Second + stateWaitMargin)
	policy := s.newBackoff(ctx)
	for {
		state, err := s.matcherRuntime.State(ctx)
		if err == nil {
			return state, nil
		}
		if !stderrors.Is(err, runtime.ErrPluginUnavailable) || time.Now().After(deadline) || !wait(ctx, policy) {
			return snapshot.ProjectionState[*matchers.MatcherMetadata]{}, err
		}
	}
}

// decode turns every input into ordered event state; invalid records become prepared DLQ records.
func (s *Service) decode(b *batch) {
	b.states = make([]*eventState, len(b.msgs))
	for i, msg := range b.msgs {
		state := &eventState{source: msg}
		b.states[i] = state
		var evt events.Event
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			prepared := s.prepareDLQ(msg, "decode", err.Error(), 0)
			state.prepared = &prepared
			continue
		}
		logType, ok := evt["log_type"].(string)
		if !ok {
			prepared := s.prepareDLQ(msg, "log_type", "event log_type must be a string", 0)
			state.prepared = &prepared
			continue
		}

		candidates := rules.RulesForLogTypeIn(b.ruleState.Primaries, logType)
		if len(candidates) == 0 {
			drops.WithLabelValues("event", "no_rules").Inc()
			state.prepared = &preparedRecord{kind: terminalDrop}
			continue
		}

		// Encode once for matching and forwarding; encoding failures go to the DLQ without retries.
		raw, encodeErr := evt.Marshal()
		if encodeErr != nil {
			prepared := s.prepareDLQ(msg, "encode", encodeErr.Error(), 0)
			state.prepared = &prepared
			continue
		}

		state.event = evt
		state.raw = raw
		state.candidates = make([]*ruleCandidate, len(candidates))
		for i, candidate := range candidates {
			state.candidates[i] = &ruleCandidate{meta: candidate, eligible: true}
		}
	}
}

// groupByMatcher groups each event's rule candidates by required matcher.
func (s *Service) groupByMatcher(b *batch) map[string]*matcherEntry {
	byMatcher := make(map[string]*matcherEntry)
	for _, state := range b.states {
		if state.prepared != nil {
			continue
		}
		perMatcher := make(map[string][]*ruleCandidate)
		metadata := make(map[string]*matchers.MatcherMetadata)
		for _, candidate := range state.candidates {
			for _, name := range candidate.meta.Matchers {
				matcher, ok := b.matcherState.ByFileName[name]
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

// recordFailure keeps the lowest matcher identifier's failure for reproducible dead-letter reasons.
func (s *eventState) recordFailure(matcher, reason string, attempts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil || matcher < s.failure.matcher {
		s.failure = &matcherFailure{matcher: matcher, reason: reason, attempts: attempts}
	}
}

// evaluateMatchers resolves matcher groups concurrently and reports unavailable calls.
func (s *Service) evaluateMatchers(ctx context.Context, b *batch) bool {
	var unavailable atomic.Bool
	var wg sync.WaitGroup
	for _, entry := range s.groupByMatcher(b) {
		wg.Go(func() {
			if s.matchWithRetries(ctx, b, entry) {
				unavailable.Store(true)
			}
		})
	}
	wg.Wait()
	return unavailable.Load()
}

// matchWithRetries retries failed items, records exhausted failures, and reports unavailable calls.
func (s *Service) matchWithRetries(ctx context.Context, b *batch, entry *matcherEntry) bool {
	pending := entry.items
	unavailable := false
	policy := s.newBackoff(ctx)
	for attempt := 1; ; attempt++ {
		pendingEvents := make([]events.Event, len(pending))
		pendingRaw := make([][]byte, len(pending))
		for i, item := range pending {
			pendingEvents[i] = item.state.event
			pendingRaw[i] = item.state.raw
		}
		result := s.match(ctx, b, entry.meta, events.NewBatch(pendingEvents, pendingRaw), attempt > 1)
		if ctx.Err() != nil {
			return unavailable
		}

		if result.CallErr != nil {
			// Whole-call failures retry every pending item with the same dead-letter reason.
			s.logger.Error(result.CallErr)
			if stderrors.Is(result.CallErr, runtime.ErrPluginUnavailable) {
				unavailable = true
			}
			for i := range pending {
				pending[i].reason = fmt.Sprint(result.CallErr.Message())
			}
		} else {
			next := make([]matcherItem, 0, len(pending))
			for i, item := range pending {
				match := result.Items[i]
				if match.Err != nil {
					s.logger.Error(match.Err)
					item.reason = fmt.Sprint(match.Err.Message())
					next = append(next, item)
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
			pending = next
			if len(pending) == 0 {
				return unavailable
			}
		}

		if attempt >= s.config.MaxAttempts {
			for _, item := range pending {
				item.state.recordFailure(entry.meta.Id, item.reason, attempt)
			}
			return unavailable
		}
		if !wait(ctx, policy) {
			return unavailable
		}
	}
}

// match performs one bounded, timed runtime call for the pending items.
func (s *Service) match(ctx context.Context, b *batch, matcher *matchers.MatcherMetadata, pending *events.Batch, retry bool) matchers.MatchResult {
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return matchers.MatchResult{CallErr: errors.NewE(err)}
	}
	if retry {
		evaluationRetry.WithLabelValues(matcher.Name).Inc()
	}
	evaluationsLive.Inc()
	defer func() {
		evaluationsLive.Dec()
		s.sem.Release(1)
	}()

	matchCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	result := s.matcherRuntime.Match(matchCtx, b.matcherState, matcher.Id, pending)
	evaluationTime.WithLabelValues(matcher.Name).Observe(time.Since(start).Seconds())
	callResult := metricResult(ctx, result.CallErr)
	itemErrors := 0
	if result.CallErr == nil && len(result.Items) != pending.Len() {
		result = matchers.MatchResult{CallErr: errors.NewF("matcher %s returned invalid result shape", matcher.Name)}
		callResult = "error"
	}
	if result.CallErr != nil {
		evaluationItems.WithLabelValues(matcher.Name, "error").Add(float64(pending.Len()))
	} else {
		for _, item := range result.Items {
			itemResult := "unmatched"
			if item.Err != nil {
				itemResult = "error"
				itemErrors++
			} else if item.Matched {
				itemResult = "matched"
			}
			evaluationItems.WithLabelValues(matcher.Name, itemResult).Inc()
		}
		if itemErrors > 0 {
			callResult = "error"
		}
	}
	evaluationTotal.WithLabelValues(matcher.Name, callResult).Inc()
	return result
}

// prepare resolves every event to a terminal record before any broker write begins.
func (s *Service) prepare(b *batch) {
	for _, state := range b.states {
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
			drops.WithLabelValues("event", "unmatched").Inc()
			state.prepared = &preparedRecord{kind: terminalDrop}
			continue
		}

		state.prepared = &preparedRecord{kind: terminalNormal, message: brokers.Message{
			Key: append([]byte(nil), state.source.Key...), Value: execPayload(state.raw, ruleIDs),
		}}
	}
}

// publish preserves fetched order, batching consecutive records bound for the same writer.
func (s *Service) publish(ctx context.Context, b *batch) errors.Error {
	var (
		pending       []brokers.Message
		pendingStages []string
		pendingKind   terminalKind
	)
	flush := func() errors.Error {
		if len(pending) == 0 {
			return nil
		}
		writer := s.executorWriter
		destination := "output"
		if pendingKind == terminalDLQ {
			writer = s.dlqWriter
			destination = "dlq"
		}
		if err := s.writeWithRetries(ctx, writer, destination, pending...); err != nil {
			return err
		}
		recordsOut.WithLabelValues(destination).Add(float64(len(pending)))
		if pendingKind == terminalDLQ {
			for _, stage := range pendingStages {
				dlqRecords.WithLabelValues(stage).Inc()
			}
		}
		pending = nil
		pendingStages = nil
		return nil
	}
	for _, state := range b.states {
		if state.prepared == nil {
			return errors.NewF("event matcher left an input without a terminal state")
		}
		kind := state.prepared.kind
		if kind == terminalDrop {
			continue
		}
		if len(pending) > 0 && kind != pendingKind {
			if err := flush(); err != nil {
				return err
			}
		}
		pendingKind = kind
		pending = append(pending, state.prepared.message)
		if kind == terminalDLQ {
			pendingStages = append(pendingStages, state.prepared.stage)
		}
	}
	return flush()
}

// writeWithRetries bounds each publish so Runner can restart a failed service attempt.
func (s *Service) writeWithRetries(ctx context.Context, w brokers.Writer, destination string, msgs ...brokers.Message) errors.Error {
	policy := s.newBackoff(ctx)
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			writeRetry.WithLabelValues(destination).Inc()
		}
		start := time.Now()
		err := w.WriteMessages(ctx, msgs...)
		writeTime.WithLabelValues(destination).Observe(time.Since(start).Seconds())
		writeTotal.WithLabelValues(destination, metricResult(ctx, err)).Inc()
		if err == nil {
			return nil
		}
		if attempt >= s.config.MaxAttempts || !wait(ctx, policy) {
			return errors.NewE(err)
		}
	}
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

// newBackoff returns a jittered exponential policy; callers bound attempts and elapsed time.
func (s *Service) newBackoff(ctx context.Context) backoff.BackOffContext {
	return backoff.WithContext(backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(time.Duration(s.config.RetryBaseMS)*time.Millisecond),
		backoff.WithMaxInterval(time.Duration(s.config.RetryCapMS)*time.Millisecond),
		backoff.WithMaxElapsedTime(0),
	), ctx)
}

// wait reports whether the next retry delay elapsed without cancellation or backoff exhaustion.
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

// ExecMessage field numbers, from internal/exec/exec.proto.
const (
	execEventField   = 1
	execRuleIDsField = 2
)

// execPayload builds an ExecMessage using the event encoding already used by matcher calls.
func execPayload(event []byte, ruleIDs []string) []byte {
	size := protowire.SizeTag(execEventField) + protowire.SizeBytes(len(event))
	for _, id := range ruleIDs {
		size += protowire.SizeTag(execRuleIDsField) + protowire.SizeBytes(len(id))
	}
	out := make([]byte, 0, size)
	out = protowire.AppendTag(out, execEventField, protowire.BytesType)
	out = protowire.AppendBytes(out, event)
	for _, id := range ruleIDs {
		out = protowire.AppendTag(out, execRuleIDsField, protowire.BytesType)
		out = protowire.AppendString(out, id)
	}
	return out
}

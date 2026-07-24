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
	dlqpb "github.com/harishhary/blink/internal/dlq/pb"
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
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	eventsIn        = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "events_in_total"})
	eventsForwarded = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "events_forwarded_total"})
	readErrors      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "read_errors_total"})
	parseErrors     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "parse_errors_total"})
	writeErrors     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "write_errors_total"})
	matchDuration   = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "match_duration_seconds", Buckets: prometheus.DefBuckets})
	rulesRouted     = promauto.NewHistogram(prometheus.HistogramOpts{Namespace: "blink", Subsystem: "event_matcher", Name: "rules_routed_per_event", Buckets: []float64{0, 1, 5, 10, 25, 50, 100}})
)

// Service prepares a terminal noop, normal record, or DLQ record for every
// fetched event before publishing normal and DLQ records in fetched order.
type Service struct {
	logger     *logger.Logger
	config     Config
	execWriter brokers.Writer
	dlqWriter  brokers.Writer
	ruleCfg    *rules.SnapshotConfig // rule controller's snapshot - the rollout authority
	pool       *matchers.Pool
	sem        *semaphore.Weighted // bounds parallel Pool.Match calls to MATCHER_CONCURRENCY
}

// Config is the explicit set of dependencies NewService needs, injected by main.
// Config's topic fields are populated from the environment by main (which embeds it);
// Broker is injected after load.
type Config struct {
	Broker       brokers.Broker
	EventTopic   string `env:"KAFKA_TOPIC_EVENT"`
	MatcherGroup string `env:"KAFKA_GROUP_EVENT_MATCHER"`
	ExecTopic    string `env:"KAFKA_TOPIC_EXEC"`
	DLQTopic     string `env:"KAFKA_TOPIC_MATCHER_DLQ"`
	// Ready gates grouped-consumer creation until matcher and rule snapshots catch up.
	Ready func() bool
	// Concurrency is the maximum number of parallel matcher plugin calls.
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

func NewService(logger *logger.Logger, cfg Config, pool *matchers.Pool, ruleCfg *rules.SnapshotConfig) *Service {
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
		logger:     logger,
		config:     cfg,
		execWriter: cfg.Broker.NewWriter(cfg.ExecTopic),
		dlqWriter:  cfg.Broker.NewWriter(cfg.DLQTopic),
		ruleCfg:    ruleCfg,
		pool:       pool,
		sem:        semaphore.NewWeighted(int64(cfg.Concurrency)),
	}
}

func (s *Service) Name() string { return "event-matcher" }

func (s *Service) Run(ctx context.Context) errors.Error {
	// matcher and rule snapshots need to catch up, so we never match against a half-loaded rollout state
	ticker := time.NewTicker(10 * time.Millisecond)
	for !s.config.Ready() {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return nil
		case <-ticker.C:
		}
	}
	ticker.Stop()
	if ctx.Err() != nil {
		return nil
	}

	reader := s.config.Broker.NewReader(s.config.EventTopic, s.config.MatcherGroup)
	defer func() {
		if err := reader.Close(); err != nil {
			s.logger.Error(errors.NewE(err))
		}
	}()

	for {
		msgs, err := reader.ReadBatch(ctx, 50)
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
			s.logger.Error(err)
			return err
		}

		if err := reader.CommitMessages(ctx, msgs...); err != nil {
			err := errors.NewE(err)
			s.logger.Error(err)
			return err
		}
	}
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

type matcherFailure struct {
	matcher  string
	reason   string
	attempts int
}

type eventState struct {
	source     brokers.Message
	event      events.Event
	candidates []*rules.RuleMetadata
	eligible   map[string]bool
	failures   map[string]matcherFailure
	prepared   *preparedRecord
}

type routedItem struct {
	stateIdx int
	ruleIDs  []string
}

func (s *Service) processBatch(batchCtx context.Context, msgs []brokers.Message) errors.Error {
	states, err := s.decodeStates(msgs)
	if err != nil {
		return err
	}
	s.evaluateMatchers(batchCtx, states)
	// Shutdown is the only reason evaluation leaves states unresolved; bail
	// without committing so the batch is redelivered.
	if batchCtx.Err() != nil {
		return errors.NewE(batchCtx.Err())
	}
	if err := s.prepareTerminals(states); err != nil {
		return err
	}
	return s.publishTerminals(batchCtx, states)
}

// decodeStates decodes every input into an ordered state. Invalid records become
// prepared DLQ records immediately, but nothing is published until all states are
// terminal.
func (s *Service) decodeStates(msgs []brokers.Message) ([]*eventState, errors.Error) {
	allRules := s.ruleCfg.Primaries()
	states := make([]*eventState, len(msgs))
	for i, msg := range msgs {
		state := &eventState{source: msg}
		states[i] = state
		var evt events.Event
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			parseErrors.Inc()
			prepared, prepareErr := s.prepareDLQ(msg, "decode", err.Error(), 0)
			if prepareErr != nil {
				return nil, prepareErr
			}
			state.prepared = &prepared
			continue
		}
		eventsIn.Inc()

		logType, ok := evt["log_type"].(string)
		if !ok {
			prepared, prepareErr := s.prepareDLQ(msg, "log_type", "event log_type must be a string", 0)
			if prepareErr != nil {
				return nil, prepareErr
			}
			state.prepared = &prepared
			continue
		}

		candidates := rules.RulesForLogTypeIn(allRules, logType)
		if len(candidates) == 0 {
			state.prepared = &preparedRecord{kind: terminalDrop}
			continue
		}

		state.event = evt
		state.candidates = candidates
		state.eligible = make(map[string]bool, len(candidates))
		for _, r := range candidates {
			state.eligible[r.Id] = true
		}
		state.failures = make(map[string]matcherFailure)
	}
	return states, nil
}

// groupByMatcher collects, per matcher, the pending states that need it along
// with the rule IDs that depend on the outcome. Each state contributes at most
// one item per matcher, in fetched order.
func groupByMatcher(states []*eventState) map[string][]routedItem {
	byMatcher := make(map[string][]routedItem)
	for i, s := range states {
		if s.prepared != nil {
			continue
		}
		perMatcher := make(map[string][]string)
		for _, r := range s.candidates {
			for _, name := range r.Matchers {
				perMatcher[name] = append(perMatcher[name], r.Id)
			}
		}
		for name, ruleIDs := range perMatcher {
			byMatcher[name] = append(byMatcher[name], routedItem{stateIdx: i, ruleIDs: ruleIDs})
		}
	}
	return byMatcher
}

// matcherRun coordinates one batch's concurrent matcher evaluation. Every
// failure is per-event: retried up to MaxAttempts, then recorded so
// prepareTerminals routes the event to the DLQ. Only batch context
// cancellation (shutdown) stops evaluation early.
type matcherRun struct {
	service *Service
	states  []*eventState
	ctx     context.Context
	mu      sync.Mutex // guards per-state eligible/failures mutation
}

func (s *Service) evaluateMatchers(batchCtx context.Context, states []*eventState) {
	run := &matcherRun{
		service: s,
		states:  states,
		ctx:     batchCtx,
	}

	var wg sync.WaitGroup
	for name, items := range groupByMatcher(states) {
		wg.Go(func() {
			run.matchWithRetries(name, items)
		})
	}
	wg.Wait()
}

// errPendingRetries drives the backoff between attempts; per-item outcomes
// live in run.states, never in the returned error.
var errPendingRetries = stderrors.New("matcher retries pending")

// matchWithRetries owns one matcher's failed subset across attempts.
func (r *matcherRun) matchWithRetries(name string, pending []routedItem) {
	service := r.service
	attempt := 0
	_ = backoff.Retry(func() error {
		attempt++
		result, ok := r.match(name, pending)
		if !ok {
			// Shutdown; processBatch bails without committing.
			return backoff.Permanent(errPendingRetries)
		}

		// Whole-call failures (missing plugin, call timeout, invalid shape) carry no
		// per-item detail: fail every pending item for this attempt so the shared
		// retry/DLQ path below owns them.
		callErr := result.CallErr
		if callErr == nil && len(result.Items) != len(pending) {
			callErr = errors.NewF("matcher %s returned invalid result shape", name)
		}
		if callErr != nil {
			items := make([]matchers.MatchItem, len(pending))
			for j := range items {
				items[j].Err = callErr
			}
			result = matchers.MatchResult{Items: items}
		}

		pending = r.apply(name, attempt, pending, result)
		if len(pending) == 0 {
			// Done: apply records DLQ failures for leftovers at MaxAttempts.
			return nil
		}
		return errPendingRetries
	}, service.newBackoff(r.ctx))
}

// match performs one bounded, timed Pool.Match call for the pending items.
// ok is false when the batch or evaluation context ended the run.
func (r *matcherRun) match(name string, pending []routedItem) (matchers.MatchResult, bool) {
	if err := r.service.sem.Acquire(r.ctx, 1); err != nil {
		return matchers.MatchResult{}, false
	}
	defer r.service.sem.Release(1)

	evts := make([]events.Event, len(pending))
	for j, item := range pending {
		evts[j] = r.states[item.stateIdx].event
	}
	matchCtx, cancelMatch := context.WithTimeout(r.ctx, time.Duration(r.service.config.TimeoutSec)*time.Second)
	defer cancelMatch()
	start := time.Now()
	result := r.service.pool.Match(matchCtx, name, evts)
	matchDuration.Observe(time.Since(start).Seconds())

	if r.ctx.Err() != nil {
		return matchers.MatchResult{}, false
	}
	return result, true
}

// apply records per-item outcomes and returns the subset to retry.
func (r *matcherRun) apply(name string, attempt int, pending []routedItem, result matchers.MatchResult) []routedItem {
	next := make([]routedItem, 0, len(pending))
	r.mu.Lock()
	defer r.mu.Unlock()
	for j, item := range pending {
		match := result.Items[j]
		state := r.states[item.stateIdx]
		if match.Err != nil {
			r.service.logger.Error(match.Err)
			if attempt == r.service.config.MaxAttempts {
				state.failures[name] = matcherFailure{
					matcher: name, reason: fmt.Sprint(match.Err.Message()), attempts: attempt,
				}
			} else {
				next = append(next, item)
			}
			continue
		}
		if !match.Matched {
			for _, ruleID := range item.ruleIDs {
				state.eligible[ruleID] = false
			}
		}
	}
	return next
}

func (s *Service) prepareTerminals(states []*eventState) errors.Error {
	for _, state := range states {
		if state.prepared != nil {
			continue
		}
		if len(state.failures) > 0 {
			// Deterministic pick: the alphabetically first failing matcher.
			var failure matcherFailure
			for _, f := range state.failures {
				if failure.matcher == "" || f.matcher < failure.matcher {
					failure = f
				}
			}
			prepared, err := s.prepareDLQ(state.source, "matcher", fmt.Sprintf("matcher %s: %s", failure.matcher, failure.reason), failure.attempts)
			if err != nil {
				return err
			}
			state.prepared = &prepared
			continue
		}

		var ruleIDs []string
		for _, r := range state.candidates {
			if state.eligible[r.Id] {
				ruleIDs = append(ruleIDs, r.Id)
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

// publishTerminals publishes prepared bytes serially in fetched order. A failed
// acknowledgement retries the same bytes indefinitely; matcher evaluation is
// never repeated.
func (s *Service) publishTerminals(ctx context.Context, states []*eventState) errors.Error {
	for _, state := range states {
		if state.prepared == nil {
			return errors.NewF("event matcher left an input without a terminal state")
		}
		if state.prepared.kind == terminalDrop {
			continue
		}
		writer := s.execWriter
		if state.prepared.kind == terminalDLQ {
			writer = s.dlqWriter
		}
		err := backoff.Retry(func() error {
			if werr := writer.WriteMessages(ctx, state.prepared.message); werr != nil {
				writeErrors.Inc()
				return werr
			}
			return nil
		}, s.newBackoff(ctx))
		if err != nil {
			return errors.NewE(err) // only ctx cancellation stops the write retry
		}
		if state.prepared.kind == terminalNormal {
			eventsForwarded.Inc()
		}
	}
	return nil
}

func (s *Service) prepareDLQ(source brokers.Message, stage, reason string, attempts int) (preparedRecord, errors.Error) {
	payload, err := proto.Marshal(&dlqpb.DLQEnvelope{
		Source: &dlqpb.DLQSource{
			Topic: source.Topic, Partition: int32(source.Partition), Offset: source.Offset,
		},
		OriginalPayload: source.Value,
		Stage:           stage,
		Reason:          reason,
		Attempts:        int32(attempts),
		FailedAt:        timestamppb.Now(),
	})
	if err != nil {
		return preparedRecord{}, errors.NewE(err)
	}
	return preparedRecord{kind: terminalDLQ, message: brokers.Message{
		Key: append([]byte(nil), source.Key...), Value: payload,
	}}, nil
}

// newBackoff returns the service's exponential retry policy (RetryBaseMS
// initial, RetryCapMS interval cap, jittered), aborted when ctx ends.
func (s *Service) newBackoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = time.Duration(s.config.RetryBaseMS) * time.Millisecond
	b.MaxInterval = time.Duration(s.config.RetryCapMS) * time.Millisecond
	b.MaxElapsedTime = 0 // callers bound attempts themselves or retry until ctx cancel
	return backoff.WithContext(b, ctx)
}

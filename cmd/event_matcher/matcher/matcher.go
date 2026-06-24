package matcher

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/configuration"
	ctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	execpb "github.com/harishhary/blink/internal/exec/pb"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
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

// MatcherService routes incoming events to eligible rules and publishes ExecMessages
// to blink-exec. For each batch it:
//  1. Decodes all events and finds candidate rules by log_type.
//  2. Groups events by matcher plugin and calls each matcher once with all relevant events,
//     filtering candidates down to the eligible set (rules whose every matcher passed).
//  3. Emits one ExecMessage per event with the eligible rule IDs from step 2.
type MatcherService struct {
	ctx.ServiceContext
	reader     brokers.Reader
	writer     brokers.Writer
	cfgWatcher *rules.RuleConfigWatcher
	pool       *matchers.Pool
}

func NewMatcherService(pool *matchers.Pool, cfgWatcher *rules.RuleConfigWatcher) (*MatcherService, error) {
	serviceContext := ctx.New("BLINK-EVENT-MATCHER - MATCHER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	b := brokers.NewKafkaBroker(serviceContext.Configuration().Kafka)
	readr := b.NewReader(
		serviceContext.Configuration().Topics.MatcherTopic,
		serviceContext.Configuration().Topics.MatcherGroup,
	)
	writer := b.NewWriter(serviceContext.Configuration().Topics.ExecTopic)

	return &MatcherService{
		ServiceContext: serviceContext,
		reader:         readr,
		writer:         writer,
		cfgWatcher:     cfgWatcher,
		pool:           pool,
	}, nil
}

func (service *MatcherService) Name() string { return "event-matcher" }

func (service *MatcherService) Run(ctx context.Context) errors.Error {
	for {
		msgs, err := service.reader.ReadBatch(ctx, 50)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			readErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}

		service.processBatch(ctx, msgs)

		if err := service.reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			service.Error(errors.NewE(err))
		}
	}
}

// eventState holds a decoded event and the set of candidate rules still eligible.
type eventState struct {
	key        []byte
	event      events.Event
	logType    string
	candidates []*rules.RuleMetadata
	eligible   map[string]bool
}

func (service *MatcherService) processBatch(batchCtx context.Context, msgs []brokers.Message) {
	reg := service.cfgWatcher.Current()

	// Decode events and find candidate rules.
	states := make([]*eventState, 0, len(msgs))
	for _, m := range msgs {
		var evt events.Event
		if err := json.Unmarshal(m.Value, &evt); err != nil {
			parseErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		eventsIn.Inc()

		logType, ok := evt["log_type"].(string)
		if !ok {
			continue
		}

		candidates := rules.RulesForLogType(reg, logType)
		if len(candidates) == 0 {
			continue
		}

		eligible := make(map[string]bool, len(candidates))
		for _, r := range candidates {
			eligible[r.Id] = true
		}
		states = append(states, &eventState{
			key: m.Key, event: evt, logType: logType,
			candidates: candidates, eligible: eligible,
		})
	}
	if len(states) == 0 {
		return
	}

	// Group by matcher: byMatcher[name] = one slot per (matcher, event) pair.
	// slotOf[name][stateIdx] tracks where that slot lives so multiple rules sharing
	// the same matcher for the same event are merged into one slot rather than
	// calling pool.Match with the same event twice.
	type matchItem struct {
		stateIdx int
		ruleIDs  []string // rules that require this matcher for this event
	}
	byMatcher := make(map[string][]matchItem)
	slotOf := make(map[string]map[int]int) // matcher → stateIdx → position in byMatcher[matcher]
	for i, s := range states {
		for _, r := range s.candidates {
			for _, name := range r.Matchers() {
				if slotOf[name] == nil {
					slotOf[name] = make(map[int]int)
				}
				if pos, ok := slotOf[name][i]; ok {
					byMatcher[name][pos].ruleIDs = append(byMatcher[name][pos].ruleIDs, r.Id)
				} else {
					slotOf[name][i] = len(byMatcher[name])
					byMatcher[name] = append(byMatcher[name], matchItem{stateIdx: i, ruleIDs: []string{r.Id}})
				}
			}
		}
	}

	// Fan out: one goroutine per matcher with all its events.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, items := range byMatcher {
		wg.Add(1)
		go func(name string, items []matchItem) {
			defer wg.Done()

			evts := make([]events.Event, len(items))
			for j, item := range items {
				evts[j] = states[item.stateIdx].event
			}

			results, err := service.pool.Match(batchCtx, name, evts, "")
			if err != nil {
				// On error, fail all affected (event, rule) pairs conservatively.
				service.Error(err)
				mu.Lock()
				for _, item := range items {
					for _, ruleID := range item.ruleIDs {
						states[item.stateIdx].eligible[ruleID] = false
					}
				}
				mu.Unlock()
				return
			}

			mu.Lock()
			for j, item := range items {
				if !results[j] {
					for _, ruleID := range item.ruleIDs {
						states[item.stateIdx].eligible[ruleID] = false
					}
				}
			}
			mu.Unlock()
		}(name, items)
	}
	wg.Wait()

	// Write ExecMessages - one per event with its eligible rule IDs.
	var outWg sync.WaitGroup
	for _, s := range states {
		outWg.Add(1)
		go func(s *eventState) {
			defer outWg.Done()

			start := time.Now()
			var ruleIDs []string
			for _, r := range s.candidates {
				if s.eligible[r.Id] {
					ruleIDs = append(ruleIDs, r.Id)
				}
			}
			matchDuration.Observe(time.Since(start).Seconds())
			rulesRouted.Observe(float64(len(ruleIDs)))

			if len(ruleIDs) == 0 {
				return
			}
			service.Info("routing event log_type=%s to %d rule(s)", s.logType, len(ruleIDs))

			eventStruct, err := structpb.NewStruct(s.event)
			if err != nil {
				parseErrors.Inc()
				service.Error(errors.NewE(err))
				return
			}
			payload, _ := proto.Marshal(&execpb.ExecMessage{
				Event:   eventStruct,
				RuleIds: ruleIDs,
			})
			if err := service.writer.WriteMessages(batchCtx, brokers.Message{Key: s.key, Value: payload}); err != nil {
				writeErrors.Inc()
				service.Error(errors.NewE(err))
			} else {
				eventsForwarded.Inc()
			}
		}(s)
	}
	outWg.Wait()
}

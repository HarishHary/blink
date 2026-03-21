package matcher

import (
	"context"
	"encoding/json"
	"time"

	bkr "github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	ctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	execpb "github.com/harishhary/blink/internal/exec/pb"
	"github.com/harishhary/blink/internal/logger"
	matchcatalog "github.com/harishhary/blink/pkg/matchers/pool"
	"github.com/harishhary/blink/pkg/rules/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	proto "google.golang.org/protobuf/proto"
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
// to blink-exec. For each event it:
//  1. Looks up candidate rules by log_type from the YAML config registry.
//  2. For each candidate, runs matcher plugins (e.g. prod-accounts) via the pool.
//  3. Emits one ExecMessage per event containing the event JSON and eligible rule IDs.
//
// The rule_executor pod evaluates only the rules in ExecMessage.RuleIDs, avoiding
// unnecessary subprocess invocations for rules that don't apply to this event.
type MatcherService struct {
	ctx.ServiceContext
	reader     bkr.Reader
	writer     bkr.Writer
	cfgWatcher *config.Watcher
	pool       *matchcatalog.Pool
}

func NewMatcherService(pool *matchcatalog.Pool, cfgWatcher *config.Watcher) (*MatcherService, error) {
	serviceContext := ctx.New("BLINK-EVENT-MATCHER - MATCHER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	b := kafka.NewKafkaBroker(serviceContext.Configuration().Kafka)
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

// Run reads raw events, routes them to eligible rules via matcher checks, and writes ExecMessages to blink-exec.
func (service *MatcherService) Run(ctx context.Context) errors.Error {
	for {
		msg, err := service.reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			readErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		eventsIn.Inc()

		var evt map[string]any
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			parseErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}

		logType, ok := evt["log_type"].(string)
		if !ok {
			continue
		}

		start := time.Now()
		ruleIDs := service.route(ctx, evt, logType)
		matchDuration.Observe(time.Since(start).Seconds())
		rulesRouted.Observe(float64(len(ruleIDs)))

		if len(ruleIDs) == 0 {
			continue
		}

		service.Info("routing event log_type=%s to %d rule(s)", logType, len(ruleIDs))

		eventStruct, err := structpb.NewStruct(evt)
		if err != nil {
			parseErrors.Inc()
			service.Error(errors.NewE(err))
			continue
		}
		payload, _ := proto.Marshal(&execpb.ExecMessage{
			Event:   eventStruct,
			RuleIds: ruleIDs,
		})
		if err := service.writer.WriteMessages(ctx, bkr.Message{Key: msg.Key, Value: payload}); err != nil {
			writeErrors.Inc()
			service.Error(errors.NewE(err))
		} else {
			eventsForwarded.Inc()
		}
	}
}

// returns the IDs of rules that are eligible for this event based on:
//  1. log_type matching (rules with empty log_types match all)
//  2. matcher plugin checks (rules with no matchers match all)
func (service *MatcherService) route(ctx context.Context, evt map[string]any, logType string) []string {
	reg := service.cfgWatcher.Current()
	candidates := reg.RulesForLogType(logType)

	var ruleIDs []string
	for _, rule := range candidates {
		if service.applyMatchers(ctx, evt, rule.Matchers()) {
			ruleIDs = append(ruleIDs, rule.Id())
		}
	}
	return ruleIDs
}

// applyMatchers runs the named matcher plugins against the event via the pool.
// Returns true when all matchers pass (or when there are no matchers).
func (service *MatcherService) applyMatchers(ctx context.Context, evt map[string]any, matcherNames []string) bool {
	for _, name := range matcherNames {
		ok, err := service.pool.Match(ctx, name, evt, "")
		if err != nil || !ok {
			return false
		}
	}
	return true
}

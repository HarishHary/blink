package merger

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/broker/kafka"
	"github.com/harishhary/blink/internal/configuration"
	svcctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	alertsIn      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "alerts_in_total"})
	alertsOut     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "alerts_out_total"})
	alertsMerged  = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "alerts_merged_total"})
	groupsFlushed = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "groups_flushed_total"})
	parseErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "parse_errors_total"})
	writeErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "write_errors_total"})
	activeGroups  = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "active_groups"})
)

// mergeGroup holds a set of alerts that share the same rule and merge-by key values and are within each other's merge window.
type mergeGroup struct {
	alerts  []*alerts.Alert
	expires time.Time // oldest.Created + MergeWindowMins
}

// MergerService reads alerts from Kafka, merges related alerts within their time window, and writes merged (or pass-through) alerts to the tuner topic.
type MergerService struct {
	svcctx.ServiceContext
	reader broker.Reader
	writer broker.Writer
	mu     sync.Mutex
	groups map[string]*mergeGroup // key: rule_name|merge_by_values
}

func NewMergerService() (*MergerService, error) {
	serviceContext := svcctx.New("BLINK-ALERT-MERGER - MERGER")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		return nil, err
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	cfg := serviceContext.Configuration()
	b := kafka.NewKafkaBroker(cfg.Kafka)
	reader := b.NewReader(cfg.Topics.MergerTopic, cfg.Topics.MergerGroup)
	writer := b.NewWriter(cfg.Topics.TunerTopic)

	return &MergerService{
		ServiceContext: serviceContext,
		reader:         reader,
		writer:         writer,
		groups:         make(map[string]*mergeGroup),
	}, nil
}

func (s *MergerService) Name() string { return "alert-merger" }

// Reads alerts from MergerTopic, accumulates related alerts into merge groups, flushes expired groups to TunerTopic, and commits Kafka offsets.
func (s *MergerService) Run(ctx context.Context) errors.Error {
	// Periodic flush: every 10s, flush any merge group whose window has expired.
	go s.flushLoop(ctx)

	for {
		msgs, err := s.reader.ReadBatch(ctx, 50)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.Error(errors.NewE(err))
			continue
		}

		for _, m := range msgs {
			alert, err := alerts.Unmarshal(m.Value)
			if err != nil {
				parseErrors.Inc()
				s.Error(errors.NewE(err))
				continue
			}
			alertsIn.Inc()

			if !alert.MergeEnabled() {
				// No merge keys configured for this rule - pass straight through.
				s.writeAlert(ctx, alert)
				continue
			}

			s.accumulate(ctx, alert)
		}

		if err := s.reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.Error(errors.NewE(err))
		}
	}
}

// adds alert to its merge group, or flushes the existing group and starts a new one when the incoming alert falls outside the current window.
func (s *MergerService) accumulate(ctx context.Context, alert *alerts.Alert) {
	key := groupKey(alert)

	s.mu.Lock()
	g, exists := s.groups[key]
	if exists && g.alerts[0].CanMerge(alert) {
		g.alerts = append(g.alerts, alert)
		alertsMerged.Inc()
		s.mu.Unlock()
		return
	}

	// Either no existing group or the window has moved on - flush the old group
	// (if any) and start a new one.
	var toFlush *mergeGroup
	if exists {
		toFlush = g
	}
	s.groups[key] = &mergeGroup{
		alerts:  []*alerts.Alert{alert},
		expires: alert.Created.Add(alert.Rule.MergeWindowMins()),
	}
	activeGroups.Set(float64(len(s.groups)))
	s.mu.Unlock()

	if toFlush != nil {
		s.flushGroup(ctx, toFlush)
	}
}

// ticks every 10 seconds and flushes any group whose window has closed.
func (s *MergerService) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flushExpired(ctx)
		case <-ctx.Done():
			// Best-effort drain: attempt to flush remaining groups before exit.
			s.flushAll(context.Background())
			return
		}
	}
}

// flushes all groups whose expiry time has passed.
func (s *MergerService) flushExpired(ctx context.Context) {
	now := time.Now()

	s.mu.Lock()
	var expired []*mergeGroup
	for key, g := range s.groups {
		if now.After(g.expires) {
			expired = append(expired, g)
			delete(s.groups, key)
		}
	}
	activeGroups.Set(float64(len(s.groups)))
	s.mu.Unlock()

	for _, g := range expired {
		s.flushGroup(ctx, g)
	}
}

// flushes every remaining merge group regardless of expiry.
func (s *MergerService) flushAll(ctx context.Context) {
	s.mu.Lock()
	remaining := make([]*mergeGroup, 0, len(s.groups))
	for _, g := range s.groups {
		remaining = append(remaining, g)
	}
	s.groups = make(map[string]*mergeGroup)
	activeGroups.Set(0)
	s.mu.Unlock()

	for _, g := range remaining {
		s.flushGroup(ctx, g)
	}
}

// merges the group's alerts into one (or passes through a singleton) and writes it to the tuner topic.
func (s *MergerService) flushGroup(ctx context.Context, g *mergeGroup) {
	groupsFlushed.Inc()

	if len(g.alerts) == 1 {
		s.writeAlert(ctx, g.alerts[0])
		return
	}

	s.Info("merging %d alerts for rule %s", len(g.alerts), g.alerts[0].Rule.Name())
	merged, err := alerts.Merge(g.alerts)
	if err != nil {
		s.Error(err)
		// Fall back: write each alert individually rather than losing them.
		for _, a := range g.alerts {
			s.writeAlert(ctx, a)
		}
		return
	}
	s.writeAlert(ctx, merged)
}

// serialises alert and writes it to the tuner topic.
func (s *MergerService) writeAlert(ctx context.Context, alert *alerts.Alert) {
	payload, err := alerts.Marshal(alert)
	if err != nil {
		writeErrors.Inc()
		s.Error(errors.NewE(err))
		return
	}
	if err := s.writer.WriteMessages(ctx, broker.Message{Value: payload}); err != nil {
		writeErrors.Inc()
		s.Error(errors.NewE(err))
		return
	}
	alertsOut.Inc()
}

// groupKey builds a stable string key from the alert's rule name and merge-by field values. Keys are sorted before joining to ensure map key consistency regardless of iteration order.
func groupKey(alert *alerts.Alert) string {
	keys := alert.Rule.MergeByKeys()
	sort.Strings(keys)
	merged := alert.Event.GetMergedKeys(keys)
	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, alert.Rule.Name())
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%v", merged[k]))
	}
	return strings.Join(parts, "|")
}

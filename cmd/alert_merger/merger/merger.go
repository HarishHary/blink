package merger

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	alertsIn      = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "alerts_in_total"})
	alertsOut     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "alerts_out_total"})
	alertsDLQ     = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "alerts_dlq_total"})
	alertsMerged  = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "alerts_merged_total"})
	groupsFlushed = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "groups_flushed_total"})
	groupsEvicted = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "groups_evicted_total"})
	parseErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "parse_errors_total"})
	readErrors    = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "read_errors_total"})
	commitErrors  = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "commit_errors_total"})
	writeErrors   = promauto.NewCounter(prometheus.CounterOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "write_errors_total"})
	activeGroups  = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "active_groups"})
	pendingAlerts = promauto.NewGauge(prometheus.GaugeOpts{Namespace: "blink", Subsystem: "alert_merger", Name: "pending_alerts"})
)

type alertState struct {
	source   brokers.Message
	alert    *alerts.Alert
	resolved bool
}

type mergeGroup struct {
	key      []byte
	states   []*alertState
	oldest   time.Time
	newest   time.Time
	deadline time.Time
	expires  time.Time
}

type partitionState struct {
	states          []*alertState
	committedOffset int64
}

type runtimeState struct {
	groups     map[string]*mergeGroup
	partitions map[int]*partitionState
	tracked    map[string]struct{}
	pending    int
}

type preparedOutput struct {
	message brokers.Message
	states  []*alertState
	dlq     bool
}

// Service merges related alerts to one acknowledged output before committing their source offsets.
type Service struct {
	logger      *logger.Logger
	config      Config
	tunerWriter brokers.Writer
	dlqWriter   brokers.Writer
}

// Config contains the environment-loaded settings and runtime dependencies injected by main.
type Config struct {
	Broker            brokers.Broker
	MergerTopic       string `env:"KAFKA_TOPIC_MERGER"`
	MergerGroup       string `env:"KAFKA_GROUP_MERGER"`
	TunerTopic        string `env:"KAFKA_TOPIC_TUNER"`
	DLQTopic          string `env:"KAFKA_TOPIC_MERGER_DLQ"`
	BatchSize         int    `env:"MERGER_BATCH_SIZE,optional"`
	MaxGroups         int    `env:"MERGER_MAX_GROUPS,optional"`
	MaxPendingRecords int    `env:"MERGER_MAX_PENDING_RECORDS,optional"`
	FlushIntervalSec  int    `env:"MERGER_FLUSH_INTERVAL_SEC,optional"`
	DrainTimeoutSec   int    `env:"MERGER_DRAIN_TIMEOUT_SEC,optional"`
	RetryBaseMS       int    `env:"MERGER_RETRY_BASE_MS,optional"`
	RetryCapMS        int    `env:"MERGER_RETRY_CAP_MS,optional"`
}

func NewService(logger *logger.Logger, cfg Config) *Service {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.MaxGroups <= 0 {
		cfg.MaxGroups = 10000
	}
	if cfg.MaxPendingRecords <= 0 {
		cfg.MaxPendingRecords = 10000
	}
	if cfg.FlushIntervalSec <= 0 {
		cfg.FlushIntervalSec = 10
	}
	if cfg.DrainTimeoutSec <= 0 {
		cfg.DrainTimeoutSec = 30
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
		logger:      logger,
		config:      cfg,
		tunerWriter: cfg.Broker.NewWriter(cfg.TunerTopic),
		dlqWriter:   cfg.Broker.NewWriter(cfg.DLQTopic),
	}
}

func (s *Service) Name() string { return "alert-merger" }

func (s *Service) Run(ctx context.Context) errors.Error {
	reader := s.config.Broker.NewReader(s.config.MergerTopic, s.config.MergerGroup)
	defer func() {
		if err := reader.Close(); err != nil {
			s.logger.Error(errors.NewE(err))
		}
	}()

	state := &runtimeState{
		groups:     make(map[string]*mergeGroup),
		partitions: make(map[int]*partitionState),
		tracked:    make(map[string]struct{}),
	}
	for {
		if ctx.Err() != nil {
			s.drain(reader, state)
			return nil
		}

		if state.pending >= s.config.MaxPendingRecords {
			if !waitForFlush(ctx, s.nextFlushWait(state, time.Now())) {
				continue
			}
		} else {
			readCtx, cancel := context.WithTimeout(ctx, s.nextFlushWait(state, time.Now()))
			msgs, err := reader.ReadBatch(readCtx, min(s.config.BatchSize, s.config.MaxPendingRecords-state.pending))
			cancel()

			if ctx.Err() != nil {
				continue
			}
			if err != nil && !stderrors.Is(err, context.DeadlineExceeded) {
				readErrors.Inc()
				err := errors.NewE(err)
				s.logger.Error(err)
				return err
			}
			if len(msgs) > 0 {
				if err := s.processMessages(ctx, state, msgs); err != nil {
					if ctx.Err() != nil {
						continue
					}
					s.logger.Error(err)
					return err
				}
			}
		}

		if err := s.flushExpired(ctx, state, time.Now()); err != nil {
			if ctx.Err() != nil {
				continue
			}
			s.logger.Error(err)
			return err
		}
		if err := s.commitResolved(ctx, reader, state); err != nil {
			if ctx.Err() != nil {
				continue
			}
			commitErrors.Inc()
			err := errors.NewE(err)
			s.logger.Error(err)
			return err
		}
	}
}

func waitForFlush(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) nextFlushWait(state *runtimeState, now time.Time) time.Duration {
	wait := time.Duration(s.config.FlushIntervalSec) * time.Second
	for _, group := range state.groups {
		untilExpiry := group.expires.Sub(now)
		if untilExpiry <= 0 {
			return time.Millisecond
		}
		if untilExpiry < wait {
			wait = untilExpiry
		}
	}
	return wait
}

func (s *Service) processMessages(ctx context.Context, state *runtimeState, msgs []brokers.Message) errors.Error {
	outputs := make([]preparedOutput, 0, len(msgs))
	flushed := 0
	for _, source := range msgs {
		if state.isTracked(source) {
			continue
		}
		item := &alertState{source: source}
		state.track(item)

		alert, err := alerts.Unmarshal(source.Value)
		if err != nil {
			parseErrors.Inc()
			output, prepareErr := prepareDLQ(item, "decode", err.Error())
			if prepareErr != nil {
				return prepareErr
			}
			outputs = append(outputs, output)
			continue
		}
		if alert.Rule == nil || alert.Event == nil {
			parseErrors.Inc()
			output, prepareErr := prepareDLQ(item, "decode", "alert is missing rule or event")
			if prepareErr != nil {
				return prepareErr
			}
			outputs = append(outputs, output)
			continue
		}

		alertsIn.Inc()
		item.alert = alert
		if !alert.MergeEnabled() {
			prepared, prepareErr := s.prepareAlert(item.alert, item.source.Key, []*alertState{item})
			if prepareErr != nil {
				return prepareErr
			}
			outputs = append(outputs, prepared...)
			continue
		}

		prepared, groups, prepareErr := s.accumulate(state, item)
		if prepareErr != nil {
			return prepareErr
		}
		outputs = append(outputs, prepared...)
		flushed += groups
	}

	if err := s.publish(ctx, outputs); err != nil {
		return err
	}
	groupsFlushed.Add(float64(flushed))
	return nil
}

func (state *runtimeState) track(item *alertState) {
	partition := state.partitions[item.source.Partition]
	if partition == nil {
		partition = &partitionState{committedOffset: -1}
		state.partitions[item.source.Partition] = partition
	}
	index := sort.Search(len(partition.states), func(i int) bool {
		return partition.states[i].source.Offset > item.source.Offset
	})
	partition.states = append(partition.states, nil)
	copy(partition.states[index+1:], partition.states[index:])
	partition.states[index] = item
	state.tracked[sourceID(item.source)] = struct{}{}
	state.pending++
	pendingAlerts.Set(float64(state.pending))
}

func (state *runtimeState) isTracked(source brokers.Message) bool {
	partition := state.partitions[source.Partition]
	if partition != nil && source.Offset <= partition.committedOffset {
		return true
	}
	_, ok := state.tracked[sourceID(source)]
	return ok
}

func sourceID(source brokers.Message) string {
	return fmt.Sprintf("%s:%d:%d", source.Topic, source.Partition, source.Offset)
}

func (s *Service) accumulate(state *runtimeState, item *alertState) ([]preparedOutput, int, errors.Error) {
	key := item.alert.MergePartitionKey()
	groupID := fmt.Sprintf("%d:%s", item.source.Partition, key)
	group := state.groups[groupID]
	outputs := make([]preparedOutput, 0, 2)
	flushed := 0

	if group != nil && group.states[0].alert.CanMerge(item.alert) && groupContains(group, item.alert) {
		group.states = append(group.states, item)
		if item.alert.Created.Before(group.oldest) {
			group.oldest = item.alert.Created
		}
		if item.alert.Created.After(group.newest) {
			group.newest = item.alert.Created
		}
		group.expires = groupExpiry(group.oldest, item.alert.Rule.MergeWindowMins(), group.deadline)
		alertsMerged.Inc()
		return nil, 0, nil
	}

	if group != nil {
		prepared, err := s.prepareGroup(group)
		if err != nil {
			return nil, 0, err
		}
		outputs = append(outputs, prepared...)
		delete(state.groups, groupID)
		flushed++
	}

	now := time.Now()
	window := item.alert.Rule.MergeWindowMins()
	state.groups[groupID] = &mergeGroup{
		key:      []byte(key),
		states:   []*alertState{item},
		oldest:   item.alert.Created,
		newest:   item.alert.Created,
		deadline: now.Add(window),
		expires:  groupExpiry(item.alert.Created, window, now.Add(window)),
	}

	if len(state.groups) > s.config.MaxGroups {
		oldestID := oldestGroupID(state.groups)
		prepared, err := s.prepareGroup(state.groups[oldestID])
		if err != nil {
			return nil, 0, err
		}
		outputs = append(outputs, prepared...)
		delete(state.groups, oldestID)
		groupsEvicted.Inc()
		flushed++
	}
	activeGroups.Set(float64(len(state.groups)))
	return outputs, flushed, nil
}

func groupContains(group *mergeGroup, alert *alerts.Alert) bool {
	oldest := group.oldest
	newest := group.newest
	if alert.Created.Before(oldest) {
		oldest = alert.Created
	}
	if alert.Created.After(newest) {
		newest = alert.Created
	}
	return !newest.After(oldest.Add(alert.Rule.MergeWindowMins()))
}

func groupExpiry(oldest time.Time, window time.Duration, deadline time.Time) time.Time {
	expires := oldest.Add(window)
	if expires.After(deadline) {
		return deadline
	}
	return expires
}

func oldestGroupID(groups map[string]*mergeGroup) string {
	var oldest string
	for id, group := range groups {
		if oldest == "" || group.expires.Before(groups[oldest].expires) || group.expires.Equal(groups[oldest].expires) && id < oldest {
			oldest = id
		}
	}
	return oldest
}

func (s *Service) flushExpired(ctx context.Context, state *runtimeState, now time.Time) errors.Error {
	ids := make([]string, 0)
	for id, group := range state.groups {
		if !now.Before(group.expires) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return s.flushGroups(ctx, state, ids)
}

func (s *Service) flushAll(ctx context.Context, state *runtimeState) errors.Error {
	ids := make([]string, 0, len(state.groups))
	for id := range state.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return s.flushGroups(ctx, state, ids)
}

func (s *Service) flushGroups(ctx context.Context, state *runtimeState, ids []string) errors.Error {
	outputs := make([]preparedOutput, 0, len(ids))
	for _, id := range ids {
		prepared, err := s.prepareGroup(state.groups[id])
		if err != nil {
			return err
		}
		outputs = append(outputs, prepared...)
		delete(state.groups, id)
	}
	activeGroups.Set(float64(len(state.groups)))
	if err := s.publish(ctx, outputs); err != nil {
		return err
	}
	groupsFlushed.Add(float64(len(ids)))
	return nil
}

func (s *Service) prepareGroup(group *mergeGroup) ([]preparedOutput, errors.Error) {
	if len(group.states) == 1 {
		return s.prepareAlert(group.states[0].alert, group.key, group.states)
	}

	groupAlerts := make([]*alerts.Alert, len(group.states))
	for i, item := range group.states {
		groupAlerts[i] = item.alert
	}
	merged, err := alerts.Merge(groupAlerts)
	if err == nil {
		s.logger.Info("merging %d alerts for rule %s", len(groupAlerts), groupAlerts[0].Rule.Name)
		return s.prepareAlert(merged, group.key, group.states)
	}

	s.logger.Error(err)
	outputs := make([]preparedOutput, 0, len(group.states))
	for _, item := range group.states {
		prepared, prepareErr := s.prepareAlert(item.alert, item.source.Key, []*alertState{item})
		if prepareErr != nil {
			return nil, prepareErr
		}
		outputs = append(outputs, prepared...)
	}
	return outputs, nil
}

func (s *Service) prepareAlert(alert *alerts.Alert, key []byte, states []*alertState) ([]preparedOutput, errors.Error) {
	payload, err := alerts.Marshal(alert)
	if err == nil {
		return []preparedOutput{{
			message: brokers.Message{Key: append([]byte(nil), key...), Value: payload},
			states:  states,
		}}, nil
	}

	outputs := make([]preparedOutput, 0, len(states))
	for _, item := range states {
		output, prepareErr := prepareDLQ(item, "encode", err.Error())
		if prepareErr != nil {
			return nil, prepareErr
		}
		outputs = append(outputs, output)
	}
	return outputs, nil
}

func prepareDLQ(item *alertState, stage, reason string) (preparedOutput, errors.Error) {
	message, err := dlq.Record(item.source, stage, reason, 0)
	if err != nil {
		return preparedOutput{}, errors.NewE(err)
	}
	return preparedOutput{message: message, states: []*alertState{item}, dlq: true}, nil
}

func (s *Service) publish(ctx context.Context, outputs []preparedOutput) errors.Error {
	for _, output := range outputs {
		writer := s.tunerWriter
		if output.dlq {
			writer = s.dlqWriter
		}
		if err := s.writeWithRetries(ctx, writer, output.message); err != nil {
			return err
		}
		for _, item := range output.states {
			item.resolved = true
		}
		if output.dlq {
			alertsDLQ.Add(float64(len(output.states)))
		} else {
			alertsOut.Inc()
		}
	}
	return nil
}

func (s *Service) writeWithRetries(ctx context.Context, writer brokers.Writer, message brokers.Message) errors.Error {
	bo := s.newBackoff()
	for {
		if err := writer.WriteMessages(ctx, message); err == nil {
			return nil
		} else {
			writeErrors.Inc()
			s.logger.Error(errors.NewE(err))
		}

		delay := bo.NextBackOff()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.NewE(ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *Service) newBackoff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(time.Duration(s.config.RetryBaseMS)*time.Millisecond),
		backoff.WithMaxInterval(time.Duration(s.config.RetryCapMS)*time.Millisecond),
		backoff.WithMaxElapsedTime(0),
	)
}

func (s *Service) commitResolved(ctx context.Context, reader brokers.Reader, state *runtimeState) error {
	partitions := make([]int, 0, len(state.partitions))
	for partition := range state.partitions {
		partitions = append(partitions, partition)
	}
	sort.Ints(partitions)

	commits := make([]brokers.Message, 0, len(partitions))
	prefixes := make(map[int]int, len(partitions))
	for _, partition := range partitions {
		items := state.partitions[partition].states
		prefix := 0
		for prefix < len(items) && items[prefix].resolved {
			prefix++
		}
		if prefix > 0 {
			commits = append(commits, items[prefix-1].source)
			prefixes[partition] = prefix
		}
	}
	if len(commits) == 0 {
		return nil
	}
	if err := reader.CommitMessages(ctx, commits...); err != nil {
		return err
	}
	for partition, prefix := range prefixes {
		items := state.partitions[partition].states
		state.partitions[partition].committedOffset = items[prefix-1].source.Offset
		for _, item := range items[:prefix] {
			delete(state.tracked, sourceID(item.source))
		}
		state.partitions[partition].states = items[prefix:]
		state.pending -= prefix
	}
	pendingAlerts.Set(float64(state.pending))
	return nil
}

func (s *Service) drain(reader brokers.Reader, state *runtimeState) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.config.DrainTimeoutSec)*time.Second)
	defer cancel()
	if err := s.flushAll(ctx, state); err != nil {
		s.logger.Error(err)
		return
	}
	if err := s.commitResolved(ctx, reader, state); err != nil {
		commitErrors.Inc()
		s.logger.Error(errors.NewE(err))
	}
}

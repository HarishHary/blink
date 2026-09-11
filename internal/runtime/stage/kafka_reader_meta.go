package stage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
	"github.com/segmentio/kafka-go"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// KafkaReaderMetaLifecycle describes the reader actor-owned meta lifecycle.
type KafkaReaderMetaLifecycle string

const (
	KafkaReaderMetaStarting   KafkaReaderMetaLifecycle = "starting"
	KafkaReaderMetaRunning    KafkaReaderMetaLifecycle = "running"
	KafkaReaderMetaRestarting KafkaReaderMetaLifecycle = "restarting"
	KafkaReaderMetaStopped    KafkaReaderMetaLifecycle = "stopped"
)

// kafkaReaderMetaStatus is derived and owned by the reader actor.
type kafkaReaderMetaStatus struct {
	Lifecycle    KafkaReaderMetaLifecycle
	Availability runtime.Availability
	LastError    error
}

// kafkaReaderMeta serializes all blocking access to one reader incarnation.
type kafkaReaderMeta struct {
	gen.MetaProcess
	newReader     func() brokers.Reader
	ioBarrier     *runtime.IOBarrier
	completion    *runtime.IOBarrier
	supervisor    gen.PID
	fetchTimeout  time.Duration
	commitTimeout time.Duration
	retryMin      time.Duration
	retryMax      time.Duration
	jobs          chan any
	runCtx        context.Context
	cancelRun     context.CancelFunc
	labels        telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageKafkaReaderFetch requests a fenced broker fetch.
type MessageKafkaReaderFetch struct {
	operationID gen.Ref
	records     int
}

// MessageKafkaReaderFetchResult reports a fenced broker fetch completion.
type MessageKafkaReaderFetchResult struct {
	source      gen.Alias
	operationID gen.Ref
	records     []brokers.Message
	err         error
}

// MessageKafkaReaderCommit requests a fenced broker commit.
type MessageKafkaReaderCommit struct {
	operationID gen.Ref
	sources     []RecordRef
	positions   []brokers.Message
}

// MessageKafkaReaderCommitResult reports a fenced broker commit completion.
type MessageKafkaReaderCommitResult struct {
	source      gen.Alias
	operationID gen.Ref
	sources     []RecordRef
	err         error
}

// MessageKafkaReaderRetryProgress reports a retryable broker operation failure.
type MessageKafkaReaderRetryProgress struct {
	source      gen.Alias
	operationID gen.Ref
	kind        kafkaReaderOperationKind
	err         error
}

// MessageKafkaReaderReady reports a ready reader meta alias.
type MessageKafkaReaderReady struct{ alias gen.Alias }

// MessageKafkaReaderIOStarted reports an accepted reservation and its local completion proof.
type MessageKafkaReaderIOStarted struct {
	Alias      gen.Alias
	completion *runtime.IOBarrier
}

// MessageKafkaReaderIOStopped reports that the reader meta released its I/O reservation.
type MessageKafkaReaderIOStopped struct{ Alias gen.Alias }

// ---------------------------------------------------------------------------
// Meta lifecycle & handlers
// ---------------------------------------------------------------------------

// Init reserves this whole Start invocation before the framework launches it. Start owns release.
func (m *kafkaReaderMeta) Init(process gen.MetaProcess) error {
	if m.newReader == nil || m.ioBarrier == nil || m.fetchTimeout <= 0 || m.commitTimeout <= 0 || m.retryMin <= 0 || m.retryMax < m.retryMin {
		return fmt.Errorf("kafka reader meta: factory, I/O barrier, positive timeouts/retry minimum, and bounded retry maximum are required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.jobs = make(chan any, 1)
	m.labels.Set(m, metricKafkaReaderQueue, 0)
	if !m.ioBarrier.Acquire() {
		m.cancelRun()
		return fmt.Errorf("kafka reader meta: I/O barrier is sealed")
	}
	m.completion = runtime.NewIOBarrier()
	if !m.completion.Acquire() {
		m.cancelRun()
		m.ioBarrier.Release()
		return fmt.Errorf("kafka reader meta: reserve completion proof")
	}
	m.completion.Seal()
	if err := m.Send(m.supervisor, MessageKafkaReaderIOStarted{Alias: m.ID(), completion: m.completion}); err != nil {
		m.cancelRun()
		m.completion.Release()
		m.ioBarrier.Release()
		return fmt.Errorf("kafka reader meta: register I/O: %w", err)
	}
	return nil
}

// Start owns one fresh broker client, its serialized fetch/commit loop, and its single Close call.
func (m *kafkaReaderMeta) Start() (runErr error) {
	defer func() { _ = m.Send(m.supervisor, MessageKafkaReaderIOStopped{Alias: m.ID()}) }()
	defer m.completion.Release()
	defer m.ioBarrier.Release()
	reader := m.newReader()
	if reader == nil {
		return fmt.Errorf("kafka reader meta: factory returned nil reader")
	}
	defer func() {
		if err := reader.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("close kafka reader: %w", err)
		}
	}()
	if m.runCtx.Err() != nil {
		return nil
	}
	if err := m.Send(m.Parent(), MessageKafkaReaderReady{alias: m.ID()}); err != nil {
		return fmt.Errorf("kafka reader meta: report ready: %w", err)
	}

	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case job := <-m.jobs:
			m.labels.Set(m, metricKafkaReaderQueue, float64(len(m.jobs)))
			switch job := job.(type) {
			case MessageKafkaReaderFetch:
				if err := m.fetch(reader, job); err != nil {
					return err
				}
			case MessageKafkaReaderCommit:
				if err := m.commit(reader, job); err != nil {
					return err
				}
			}
		}
	}
}

// HandleMessage accepts only parent-issued I/O jobs and never blocks on its bounded queue.
func (m *kafkaReaderMeta) HandleMessage(from gen.PID, message any) error {
	if from != m.Parent() {
		return nil
	}
	switch message.(type) {
	case MessageKafkaReaderFetch, MessageKafkaReaderCommit:
		select {
		case m.jobs <- message:
			m.labels.Set(m, metricKafkaReaderQueue, float64(len(m.jobs)))
			return nil
		default:
			m.labels.Count(m, metricKafkaReaderQueueRejections)
			return fmt.Errorf("kafka reader meta: I/O queue full")
		}
	}
	return nil
}

// HandleCall rejects synchronous use; all broker I/O is completed by fenced messages.
func (*kafkaReaderMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("kafka reader meta: unsupported call %T", request), nil
}

// HandleInspect reports only I/O-local state; the reader actor owns lifecycle and availability.
func (m *kafkaReaderMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"kafka_reader:queue": fmt.Sprintf("%d/%d", len(m.jobs), cap(m.jobs)),
	}
}

// Terminate only requests cancellation; Start owns and closes the reader.
func (m *kafkaReaderMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// fetch executes one retrying broker fetch operation.
func (m *kafkaReaderMeta) fetch(reader brokers.Reader, job MessageKafkaReaderFetch) error {
	var records []brokers.Message
	attempt := 0
	err := backoff.RetryNotify(func() error {
		if err := m.runCtx.Err(); err != nil {
			return backoff.Permanent(err)
		}
		attempt++
		started := time.Now()
		ctx, cancel := context.WithTimeout(m.runCtx, m.fetchTimeout)
		attemptRecords, attemptErr := reader.ReadBatch(ctx, job.records)
		cancel()
		records = attemptRecords
		m.labels.Count(m, metricKafkaReaderFetches, telemetry.Result(attemptErr))
		m.labels.Observe(m, metricKafkaReaderFetchDuration, time.Since(started).Seconds())
		m.labels.Add(m, metricKafkaReaderFetchedRecords, float64(len(attemptRecords)))
		if m.runCtx.Err() != nil {
			return backoff.Permanent(m.runCtx.Err())
		}
		if attemptErr == nil {
			return nil
		}
		var kafkaError kafka.Error
		var networkError net.Error
		if len(attemptRecords) != 0 || errors.Is(attemptErr, context.Canceled) || errors.Is(attemptErr, context.DeadlineExceeded) ||
			!((errors.As(attemptErr, &kafkaError) && kafkaError.Temporary()) || (errors.As(attemptErr, &networkError) && networkError.Timeout())) {
			return backoff.Permanent(attemptErr)
		}
		return attemptErr
	}, backoff.WithContext(backoff.WithMaxRetries(backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(m.retryMin),
		backoff.WithMaxInterval(m.retryMax),
		backoff.WithMultiplier(2),
		backoff.WithMaxElapsedTime(0),
	), kafkaReaderRetryAttemptBudget-1), m.runCtx), func(err error, _ time.Duration) {
		if m.runCtx.Err() != nil {
			return
		}
		_ = m.Send(m.Parent(), MessageKafkaReaderRetryProgress{
			source: m.ID(), operationID: job.operationID,
			kind: kafkaReaderFetching, err: fmt.Errorf("fetch attempt %d/%d: %w", attempt, kafkaReaderRetryAttemptBudget, err),
		})
	})
	if m.runCtx.Err() != nil {
		return nil
	}
	result := MessageKafkaReaderFetchResult{
		source: m.ID(), operationID: job.operationID,
		records: records, err: err,
	}
	if sendErr := m.Send(m.Parent(), result); sendErr != nil {
		return fmt.Errorf("kafka reader meta: send fetch result: %w", sendErr)
	}
	return nil
}

// commit executes one retrying broker commit operation.
func (m *kafkaReaderMeta) commit(reader brokers.Reader, job MessageKafkaReaderCommit) error {
	attempt := 0
	err := backoff.RetryNotify(func() error {
		if err := m.runCtx.Err(); err != nil {
			return backoff.Permanent(err)
		}
		attempt++
		started := time.Now()
		ctx, cancel := context.WithTimeout(m.runCtx, m.commitTimeout)
		attemptErr := reader.CommitMessages(ctx, append([]brokers.Message(nil), job.positions...)...)
		cancel()
		m.labels.Count(m, metricKafkaReaderCommits, telemetry.Result(attemptErr))
		m.labels.Observe(m, metricKafkaReaderCommitDuration, time.Since(started).Seconds())
		if m.runCtx.Err() != nil {
			return backoff.Permanent(m.runCtx.Err())
		}
		if attemptErr == nil {
			return nil
		}
		var kafkaError kafka.Error
		var networkError net.Error
		if !((errors.As(attemptErr, &kafkaError) && kafkaError.Temporary()) || (errors.As(attemptErr, &networkError) && networkError.Timeout())) {
			return backoff.Permanent(attemptErr)
		}
		return attemptErr
	}, backoff.WithContext(backoff.WithMaxRetries(backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(m.retryMin),
		backoff.WithMaxInterval(m.retryMax),
		backoff.WithMultiplier(2),
		backoff.WithMaxElapsedTime(0),
	), kafkaReaderRetryAttemptBudget-1), m.runCtx), func(err error, _ time.Duration) {
		if m.runCtx.Err() != nil {
			return
		}
		_ = m.Send(m.Parent(), MessageKafkaReaderRetryProgress{
			source: m.ID(), operationID: job.operationID,
			kind: kafkaReaderCommitting, err: fmt.Errorf("commit attempt %d/%d: %w", attempt, kafkaReaderRetryAttemptBudget, err),
		})
	})
	if m.runCtx.Err() != nil {
		return nil
	}
	if err == nil {
		m.labels.Add(m, metricKafkaReaderCommitted, float64(len(job.sources)))
	}
	result := MessageKafkaReaderCommitResult{
		source: m.ID(), operationID: job.operationID,
		sources: append([]RecordRef(nil), job.sources...), err: err,
	}
	if sendErr := m.Send(m.Parent(), result); sendErr != nil {
		return fmt.Errorf("kafka reader meta: send commit result: %w", sendErr)
	}
	return nil
}

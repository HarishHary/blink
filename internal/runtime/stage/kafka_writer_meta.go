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

// KafkaWriterMetaLifecycle describes the writer actor-owned meta lifecycle.
type KafkaWriterMetaLifecycle string

const (
	KafkaWriterMetaStarting   KafkaWriterMetaLifecycle = "starting"
	KafkaWriterMetaRunning    KafkaWriterMetaLifecycle = "running"
	KafkaWriterMetaRestarting KafkaWriterMetaLifecycle = "restarting"
	KafkaWriterMetaStopped    KafkaWriterMetaLifecycle = "stopped"
)

// kafkaWriterMetaStatus is derived and owned by the writer actor.
type kafkaWriterMetaStatus struct {
	Lifecycle    KafkaWriterMetaLifecycle
	Availability runtime.Availability
	LastError    error
}

// kafkaWriterMeta owns one writer client and bounded write queue.
type kafkaWriterMeta struct {
	gen.MetaProcess
	newWriter    func() brokers.Writer
	ioBarrier    *runtime.IOBarrier
	completion   *runtime.IOBarrier
	supervisor   gen.PID
	writeTimeout time.Duration
	retryMin     time.Duration
	retryMax     time.Duration
	jobs         chan MessageKafkaWriterWrite
	runCtx       context.Context
	cancelRun    context.CancelFunc
	labels       telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageKafkaWriterWrite requests a meta-process write operation.
type MessageKafkaWriterWrite struct {
	operationID gen.Ref
	records     []brokers.Message
}

// MessageKafkaWriterWriteResult is immutable: it only identifies a completed operation and its outcome.
type MessageKafkaWriterWriteResult struct {
	source      gen.Alias
	operationID gen.Ref
	err         error
	ambiguous   bool
}

// MessageKafkaWriterRetryProgress reports a failed retry attempt.
type MessageKafkaWriterRetryProgress struct {
	source      gen.Alias
	operationID gen.Ref
	err         error
}

// MessageKafkaWriterReady reports meta-process readiness to its parent.
type MessageKafkaWriterReady struct{ alias gen.Alias }

// MessageKafkaWriterIOStarted reports an accepted reservation and its local completion proof.
type MessageKafkaWriterIOStarted struct {
	Alias      gen.Alias
	completion *runtime.IOBarrier
}

// MessageKafkaWriterIOStopped reports that the writer meta released its I/O reservation.
type MessageKafkaWriterIOStopped struct{ Alias gen.Alias }

// ---------------------------------------------------------------------------
// Meta lifecycle & handlers
// ---------------------------------------------------------------------------

// Init validates and initializes the writer meta-process.
func (m *kafkaWriterMeta) Init(process gen.MetaProcess) error {
	if m.newWriter == nil || m.ioBarrier == nil || m.writeTimeout <= 0 || m.retryMin <= 0 || m.retryMax < m.retryMin {
		return fmt.Errorf("kafka writer meta: writer factory, I/O barrier, positive timeout/retry minimum, and bounded retry maximum are required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.jobs = make(chan MessageKafkaWriterWrite, 1)
	m.labels.Set(m, metricKafkaWriterQueueDepth, 0)
	if !m.ioBarrier.Acquire() {
		m.cancelRun()
		return fmt.Errorf("kafka writer meta: I/O barrier is sealed")
	}
	m.completion = runtime.NewIOBarrier()
	if !m.completion.Acquire() {
		m.cancelRun()
		m.ioBarrier.Release()
		return fmt.Errorf("kafka writer meta: reserve completion proof")
	}
	m.completion.Seal()
	if err := m.SendWithPriority(m.supervisor, MessageKafkaWriterIOStarted{Alias: m.ID(), completion: m.completion}, gen.MessagePriorityHigh); err != nil {
		m.cancelRun()
		m.completion.Release()
		m.ioBarrier.Release()
		return fmt.Errorf("kafka writer meta: register I/O: %w", err)
	}
	return nil
}

// Start runs the writer client and processes queued writes.
func (m *kafkaWriterMeta) Start() (runErr error) {
	defer func() {
		_ = m.SendWithPriority(m.supervisor, MessageKafkaWriterIOStopped{Alias: m.ID()}, gen.MessagePriorityHigh)
	}()
	defer m.completion.Release()
	defer m.ioBarrier.Release()
	writer := m.newWriter()
	if writer == nil {
		return fmt.Errorf("kafka writer meta: writer factory returned nil")
	}
	defer func() {
		if err := writer.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("kafka writer meta: close writer: %w", err)
		}
	}()
	if m.runCtx.Err() != nil {
		return nil
	}
	if err := m.SendWithPriority(m.Parent(), MessageKafkaWriterReady{alias: m.ID()}, gen.MessagePriorityHigh); err != nil {
		return fmt.Errorf("kafka writer meta: report ready: %w", err)
	}

	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case job := <-m.jobs:
			m.labels.Set(m, metricKafkaWriterQueueDepth, float64(len(m.jobs)))
			if err := m.write(writer, job); err != nil {
				return err
			}
		}
	}
}

// HandleMessage accepts only parent-issued I/O jobs and never blocks on its bounded queue.
func (m *kafkaWriterMeta) HandleMessage(from gen.PID, message any) error {
	if from != m.Parent() {
		return nil
	}
	switch job := message.(type) {
	case MessageKafkaWriterWrite:
		select {
		case m.jobs <- job:
			m.labels.Set(m, metricKafkaWriterQueueDepth, float64(len(m.jobs)))
			return nil
		default:
			m.labels.Count(m, metricKafkaWriterQueueRejects)
			return fmt.Errorf("kafka writer meta: operation queue full")
		}
	}
	return nil
}

// HandleCall rejects unsupported meta-process calls.
func (m *kafkaWriterMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("kafka writer meta: unsupported call %T", request), nil
}

// HandleInspect returns bounded queue inspection data.
func (m *kafkaWriterMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"kafka_writer:queue": fmt.Sprintf("%d/%d", len(m.jobs), cap(m.jobs)),
	}
}

// Terminate cancels the running writer meta-process.
func (m *kafkaWriterMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// write executes one write with bounded retries and reports its result.
func (m *kafkaWriterMeta) write(writer brokers.Writer, job MessageKafkaWriterWrite) error {
	attempt := 0
	err := backoff.RetryNotify(func() error {
		if err := m.runCtx.Err(); err != nil {
			return backoff.Permanent(err)
		}
		attempt++
		started := time.Now()
		ctx, cancel := context.WithTimeout(m.runCtx, m.writeTimeout)
		attemptErr := writer.WriteMessages(ctx, job.records...)
		cancel()
		m.labels.Count(m, metricKafkaWriterOperations, telemetry.Result(attemptErr))
		m.labels.Observe(m, metricKafkaWriterOperationTime, time.Since(started).Seconds())
		if m.runCtx.Err() != nil {
			return backoff.Permanent(m.runCtx.Err())
		}
		if attemptErr == nil {
			return nil
		}
		if errors.Is(attemptErr, context.Canceled) {
			return backoff.Permanent(attemptErr)
		}
		if errors.Is(attemptErr, context.DeadlineExceeded) {
			return attemptErr
		}
		var writeErrors kafka.WriteErrors
		if errors.As(attemptErr, &writeErrors) {
			failed := false
			for _, writeErr := range writeErrors {
				if writeErr == nil {
					continue
				}
				failed = true
				if errors.Is(writeErr, context.Canceled) {
					return backoff.Permanent(attemptErr)
				}
				if errors.Is(writeErr, context.DeadlineExceeded) {
					continue
				}
				var kafkaError kafka.Error
				var networkError net.Error
				if !((errors.As(writeErr, &kafkaError) && kafkaError.Temporary()) || (errors.As(writeErr, &networkError) && networkError.Timeout())) {
					return backoff.Permanent(attemptErr)
				}
			}
			if failed {
				return attemptErr
			}
			return backoff.Permanent(attemptErr)
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
	), kafkaWriterRetryAttemptBudget-1), m.runCtx), func(err error, _ time.Duration) {
		if m.runCtx.Err() != nil {
			return
		}
		_ = m.SendWithPriority(m.Parent(), MessageKafkaWriterRetryProgress{
			source: m.ID(), operationID: job.operationID,
			err: fmt.Errorf("write attempt %d/%d: %w", attempt, kafkaWriterRetryAttemptBudget, err),
		}, gen.MessagePriorityHigh)
	})
	if m.runCtx.Err() != nil {
		return nil
	}
	if err == nil {
		m.labels.Add(m, metricKafkaWriterRecords, float64(len(job.records)))
	}
	if sendErr := m.SendWithPriority(m.Parent(), MessageKafkaWriterWriteResult{
		source: m.ID(), operationID: job.operationID, err: err, ambiguous: err != nil,
	}, gen.MessagePriorityHigh); sendErr != nil {
		return fmt.Errorf("kafka writer meta: send completion: %w", sendErr)
	}
	return nil
}

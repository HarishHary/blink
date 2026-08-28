package controller

import (
	"context"
	"fmt"
	"time"

	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// SnapshotWriterMetaLifecycle describes the controller-owned writer meta lifecycle.
type SnapshotWriterMetaLifecycle string

const (
	SnapshotWriterMetaStarting   SnapshotWriterMetaLifecycle = "starting"
	SnapshotWriterMetaRunning    SnapshotWriterMetaLifecycle = "running"
	SnapshotWriterMetaRestarting SnapshotWriterMetaLifecycle = "restarting"
	SnapshotWriterMetaStopped    SnapshotWriterMetaLifecycle = "stopped"
)

// snapshotWriterMetaStatus is derived and owned by the controller actor.
type snapshotWriterMetaStatus struct {
	Lifecycle    SnapshotWriterMetaLifecycle
	Availability runtime.Availability
	Loaded       bool
	Writing      bool
	LastError    error
}

// snapshotWriterMeta owns blocking persistence for one writer instance.
type snapshotWriterMeta struct {
	gen.MetaProcess
	database   backends.Database
	barrier    *writerIOBarrier
	supervisor gen.PID
	runCtx     context.Context
	cancelRun  context.CancelFunc
	retryMin   time.Duration
	retryMax   time.Duration
	jobs       chan MessageWriteSnapshot
	labels     telemetry.Labels
}

// --- messages ---

type MessageSnapshotLoadResult struct {
	source     gen.Alias
	records    []backends.ControllerRecord
	generation int64
	snapshot   *snapshot.Snapshot
	err        error
}

type MessageSnapshotWriteResult struct {
	source gen.Alias
	err    error
}

// MessageSnapshotWriterIOStarted proves an accepted meta Start invocation may access application resources.
type MessageSnapshotWriterIOStarted struct{ Alias gen.Alias }

// MessageSnapshotWriterIOStopped proves an accepted meta Start invocation returned.
type MessageSnapshotWriterIOStopped struct{ Alias gen.Alias }

// --- messages ---

// Init reserves writer I/O and initializes the work queue.
func (m *snapshotWriterMeta) Init(process gen.MetaProcess) error {
	if m.database == nil || m.barrier == nil {
		return fmt.Errorf("snapshot writer meta: database and barrier are required")
	}
	if !m.barrier.Acquire() {
		return fmt.Errorf("snapshot writer meta: application is stopping")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.jobs = make(chan MessageWriteSnapshot, 1)
	if err := m.Send(m.supervisor, MessageSnapshotWriterIOStarted{Alias: m.ID()}); err != nil {
		m.cancelRun()
		m.barrier.Release()
		return fmt.Errorf("snapshot writer meta: register I/O: %w", err)
	}
	m.Log().Debug("snapshot writer initialized: alias=%s supervisor=%s", m.ID(), m.supervisor)
	return nil
}

const writeRetryAttemptBudget = 5

// Start loads persisted state and writes queued updates.
func (m *snapshotWriterMeta) Start() (runErr error) {
	defer func() { _ = m.Send(m.supervisor, MessageSnapshotWriterIOStopped{Alias: m.ID()}) }()
	defer m.barrier.Release()
	defer func() {
		if runErr == nil {
			m.Log().Info("snapshot writer stopped: alias=%s", m.ID())
		}
	}()

	m.Log().Debug("snapshot writer loading persisted state: alias=%s", m.ID())

	loadStarted := time.Now()
	records, err := m.database.LoadAll(m.runCtx)
	var generation int64
	var saved *snapshot.Snapshot
	if err != nil {
		err = fmt.Errorf("%w: records: %w", runtime.ErrSnapshotLoad, err)
	} else if generation, err = m.database.LoadGeneration(m.runCtx); err != nil {
		err = fmt.Errorf("%w: generation: %w", runtime.ErrSnapshotLoad, err)
	} else if saved, err = m.database.LoadSnapshot(m.runCtx); err != nil {
		err = fmt.Errorf("%w: snapshot: %w", runtime.ErrSnapshotLoad, err)
	}
	m.labels.Observe(m, metricSnapshotLoadTime, time.Since(loadStarted).Seconds())
	m.labels.Count(m, metricSnapshotLoads, telemetry.Result(err))
	records = append([]backends.ControllerRecord(nil), records...)
	for i := range records {
		records[i] = records[i].Clone()
	}
	if err != nil {
		m.Log().Error("snapshot writer load failed: alias=%s error=%v", m.ID(), err)
	} else {
		savedEntries := 0
		if saved != nil {
			savedEntries = len(saved.Entries)
		}
		m.Log().Info("snapshot writer loaded: alias=%s records=%d generation=%d entries=%d", m.ID(), len(records), generation, savedEntries)
	}
	if sendErr := m.Send(m.Parent(), MessageSnapshotLoadResult{
		source:     m.ID(),
		records:    records,
		generation: generation,
		snapshot:   saved.Clone(),
		err:        err,
	}); sendErr != nil {
		return fmt.Errorf("%w: send result: %w", runtime.ErrSnapshotLoad, sendErr)
	}

	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case job := <-m.jobs:
			m.labels.Set(m, metricWriteQueue, float64(len(m.jobs)))
			m.Log().Debug("snapshot write started: alias=%s generation=%d changed=%t upserts=%d tombstones=%d", m.ID(), job.next.Generation, job.changed, len(job.upserts), len(job.tombstones))
			retry := backoff.WithContext(backoff.WithMaxRetries(backoff.NewExponentialBackOff(
				backoff.WithInitialInterval(m.retryMin),
				backoff.WithMaxInterval(m.retryMax),
				backoff.WithMultiplier(2),
				backoff.WithMaxElapsedTime(0),
			), writeRetryAttemptBudget-1), m.runCtx)
			var reportErr error
			attempt := 0
			err := backoff.Retry(func() error {
				attempt++
				writeErr := m.write(job)
				m.labels.Count(m, metricWriteAttempts, telemetry.Result(writeErr))
				if writeErr == nil {
					return nil
				}
				m.Log().Debug("snapshot write attempt failed: alias=%s generation=%d attempt=%d/%d error=%v", m.ID(), job.next.Generation, attempt, writeRetryAttemptBudget, writeErr)
				if sendErr := m.SendWithPriority(m.Parent(), MessageSnapshotWriteResult{source: m.ID(), err: writeErr}, gen.MessagePriorityHigh); sendErr != nil {
					reportErr = fmt.Errorf("%w: send failed attempt: %w", runtime.ErrSnapshotWrite, sendErr)
					return backoff.Permanent(reportErr)
				}
				return writeErr
			}, retry)
			if m.runCtx.Err() != nil {
				return nil
			}
			if reportErr != nil {
				return reportErr
			}
			if err != nil {
				return fmt.Errorf("snapshot write retry budget exhausted: %w", err)
			}
			if job.changed {
				m.Log().Info("snapshot write completed: alias=%s generation=%d upserts=%d tombstones=%d", m.ID(), job.next.Generation, len(job.upserts), len(job.tombstones))
			} else {
				m.Log().Debug("snapshot write skipped unchanged commit: alias=%s generation=%d records=%d", m.ID(), job.next.Generation, len(job.records))
			}
			if sendErr := m.Send(m.Parent(), MessageSnapshotWriteResult{source: m.ID()}); sendErr != nil {
				return fmt.Errorf("%w: send result: %w", runtime.ErrSnapshotWrite, sendErr)
			}
		}
	}
}

// HandleMessage queues a write for this writer instance.
func (m *snapshotWriterMeta) HandleMessage(_ gen.PID, message any) error {
	switch message := message.(type) {
	case MessageWriteSnapshot:
		select {
		case m.jobs <- message:
			m.labels.Set(m, metricWriteQueue, float64(len(m.jobs)))
			m.Log().Debug("snapshot write queued: alias=%s generation=%d", m.ID(), message.next.Generation)
			return nil
		default:
			m.labels.Count(m, metricWriteQueueRejects)
			m.Log().Warning("snapshot write queue full: alias=%s generation=%d", m.ID(), message.next.Generation)
			return fmt.Errorf("%w: already queued", runtime.ErrSnapshotWrite)
		}
	}
	return nil
}

// HandleCall rejects unsupported writer calls.
func (m *snapshotWriterMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot writer meta: unsupported call %T", request), nil
}

// HandleInspect exposes the writer's job queue depth; richer health
func (m *snapshotWriterMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"writer:queue": fmt.Sprintf("%d/%d", len(m.jobs), cap(m.jobs)),
	}
}

// Terminate cancels active writer work.
func (m *snapshotWriterMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// write persists records and, when changed, the new committed snapshot and generation.
// Subscribers learn of the change via the controller actor's own notifySubscribers push - this
// meta only owns durable persistence, not distribution.
func (m *snapshotWriterMeta) write(job MessageWriteSnapshot) error {
	if err := m.database.Upsert(m.runCtx, job.records); err != nil {
		return fmt.Errorf("%w: upsert records: %w", runtime.ErrSnapshotWrite, err)
	}
	if !job.changed {
		return nil
	}
	if err := m.database.SaveGeneration(m.runCtx, job.next.Generation); err != nil {
		return fmt.Errorf("%w: reserve generation: %w", runtime.ErrSnapshotWrite, err)
	}
	if err := m.database.SaveSnapshot(m.runCtx, job.next); err != nil {
		return fmt.Errorf("%w: save snapshot: %w", runtime.ErrSnapshotWrite, err)
	}
	return nil
}

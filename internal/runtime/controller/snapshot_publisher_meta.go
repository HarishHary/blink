package controller

import (
	"context"
	"fmt"
	"time"

	"ergo.services/ergo/gen"
	"github.com/cenkalti/backoff/v4"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// SnapshotPublisherLifecycle describes the controller-owned publisher meta lifecycle.
type SnapshotPublisherLifecycle string

const (
	SnapshotPublisherStarting   SnapshotPublisherLifecycle = "starting"
	SnapshotPublisherRunning    SnapshotPublisherLifecycle = "running"
	SnapshotPublisherRestarting SnapshotPublisherLifecycle = "restarting"
	SnapshotPublisherStopped    SnapshotPublisherLifecycle = "stopped"
)

// SnapshotPublisherStatus is derived and owned by the controller actor.
type SnapshotPublisherStatus struct {
	Lifecycle    SnapshotPublisherLifecycle
	Availability runtime.Availability
	Incarnation  uint64
	Loaded       bool
	Publishing   bool
	LastError    error
}

// snapshotPublisherMeta owns blocking persistence and broker writes for one publisher incarnation.
type snapshotPublisherMeta struct {
	gen.MetaProcess
	database    backends.Database
	writer      brokers.Writer
	barrier     *publisherIOBarrier
	incarnation uint64
	supervisor  gen.PID
	runCtx      context.Context
	cancelRun   context.CancelFunc
	jobs        chan MessagePublishSnapshot
	retryMin    time.Duration
	retryMax    time.Duration
}

// --- messages ---

type MessageSnapshotLoadResult struct {
	incarnation uint64
	records     []backends.ControllerRecord
	generation  int64
	snapshot    *snapshot.Snapshot
	err         error
}

type MessageSnapshotPublishResult struct {
	incarnation uint64
	err         error
}

const publishRetryAttemptBudget = 5

// MessageSnapshotPublisherIOStarted proves an accepted meta Start invocation may access application resources.
type MessageSnapshotPublisherIOStarted struct{ Incarnation uint64 }

// MessageSnapshotPublisherIOStopped proves an accepted meta Start invocation returned.
type MessageSnapshotPublisherIOStopped struct{ Incarnation uint64 }

// --- messages ---

// Init reserves publisher I/O and initializes the work queue.
func (m *snapshotPublisherMeta) Init(process gen.MetaProcess) error {
	if m.database == nil || m.writer == nil || m.barrier == nil {
		return fmt.Errorf("snapshot publisher meta: database, writer, and barrier are required")
	}
	if !m.barrier.Acquire() {
		return fmt.Errorf("snapshot publisher meta: application is stopping")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	m.jobs = make(chan MessagePublishSnapshot, 1)
	if err := m.Send(m.supervisor, MessageSnapshotPublisherIOStarted{Incarnation: m.incarnation}); err != nil {
		m.cancelRun()
		m.barrier.Release()
		return fmt.Errorf("snapshot publisher meta: register I/O: %w", err)
	}
	m.Log().Info("snapshot publisher initialized: incarnation=%d supervisor=%s", m.incarnation, m.supervisor)
	return nil
}

// Start loads persisted state and publishes queued updates.
func (m *snapshotPublisherMeta) Start() (runErr error) {
	defer func() { _ = m.Send(m.supervisor, MessageSnapshotPublisherIOStopped{Incarnation: m.incarnation}) }()
	defer m.barrier.Release()
	defer func() {
		if runErr != nil {
			m.Log().Error("snapshot publisher stopped: incarnation=%d error=%v", m.incarnation, runErr)
			return
		}
		m.Log().Info("snapshot publisher stopped: incarnation=%d", m.incarnation)
	}()

	m.Log().Info("snapshot publisher loading persisted state: incarnation=%d", m.incarnation)

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
	records = append([]backends.ControllerRecord(nil), records...)
	for i := range records {
		records[i] = records[i].Clone()
	}
	if err != nil {
		m.Log().Error("snapshot publisher load failed: incarnation=%d error=%v", m.incarnation, err)
	} else {
		savedEntries := 0
		if saved != nil {
			savedEntries = len(saved.Entries)
		}
		m.Log().Info("snapshot publisher loaded: incarnation=%d records=%d generation=%d entries=%d", m.incarnation, len(records), generation, savedEntries)
	}
	if sendErr := m.Send(m.Parent(), MessageSnapshotLoadResult{
		incarnation: m.incarnation,
		records:     records,
		generation:  generation,
		snapshot:    saved.Clone(),
		err:         err,
	}); sendErr != nil {
		return fmt.Errorf("%w: send result: %w", runtime.ErrSnapshotLoad, sendErr)
	}

	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case job := <-m.jobs:
			m.Log().Debug("snapshot publication started: incarnation=%d generation=%d changed=%t upserts=%d tombstones=%d", m.incarnation, job.next.Generation, job.changed, len(job.upserts), len(job.tombstones))
			retry := backoff.WithContext(backoff.WithMaxRetries(backoff.NewExponentialBackOff(
				backoff.WithInitialInterval(m.retryMin),
				backoff.WithMaxInterval(m.retryMax),
				backoff.WithMultiplier(2),
				backoff.WithMaxElapsedTime(0),
			), publishRetryAttemptBudget-1), m.runCtx)
			var reportErr error
			err := backoff.Retry(func() error {
				publishErr := m.publish(job)
				if publishErr == nil {
					return nil
				}
				m.Log().Error("snapshot publication failed: incarnation=%d generation=%d error=%v", m.incarnation, job.next.Generation, publishErr)
				if sendErr := m.SendWithPriority(m.Parent(), MessageSnapshotPublishResult{incarnation: m.incarnation, err: publishErr}, gen.MessagePriorityHigh); sendErr != nil {
					reportErr = fmt.Errorf("%w: send failed attempt: %w", runtime.ErrSnapshotPublish, sendErr)
					return backoff.Permanent(reportErr)
				}
				return publishErr
			}, retry)
			if m.runCtx.Err() != nil {
				return nil
			}
			if reportErr != nil {
				return reportErr
			}
			if err != nil {
				return fmt.Errorf("snapshot publication retry budget exhausted: %w", err)
			}
			if job.changed {
				m.Log().Info("snapshot publication completed: incarnation=%d generation=%d upserts=%d tombstones=%d", m.incarnation, job.next.Generation, len(job.upserts), len(job.tombstones))
			} else {
				m.Log().Debug("snapshot publication skipped broker write: incarnation=%d generation=%d records=%d", m.incarnation, job.next.Generation, len(job.records))
			}
			if sendErr := m.Send(m.Parent(), MessageSnapshotPublishResult{incarnation: m.incarnation}); sendErr != nil {
				return fmt.Errorf("%w: send result: %w", runtime.ErrSnapshotPublish, sendErr)
			}
		}
	}
}

// HandleMessage queues a publication for this publisher incarnation.
func (m *snapshotPublisherMeta) HandleMessage(_ gen.PID, message any) error {
	switch message := message.(type) {
	case MessagePublishSnapshot:
		if message.incarnation != m.incarnation {
			m.Log().Debug("ignored stale snapshot publication: publisher_incarnation=%d message_incarnation=%d", m.incarnation, message.incarnation)
			return nil
		}
		select {
		case m.jobs <- message:
			m.Log().Debug("snapshot publication queued: incarnation=%d generation=%d", m.incarnation, message.next.Generation)
			return nil
		default:
			m.Log().Warning("snapshot publication queue full: incarnation=%d generation=%d", m.incarnation, message.next.Generation)
			return fmt.Errorf("%w: already queued", runtime.ErrSnapshotPublish)
		}
	}
	return nil
}

// HandleCall rejects unsupported publisher calls.
func (m *snapshotPublisherMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot publisher meta: unsupported call %T", request), nil
}

// HandleInspect provides no custom publisher diagnostics.
func (m *snapshotPublisherMeta) HandleInspect(gen.PID, ...string) map[string]string { return nil }

// Terminate cancels active publisher work.
func (m *snapshotPublisherMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// publish persists records and emits changed keyed snapshot entries.
func (m *snapshotPublisherMeta) publish(job MessagePublishSnapshot) error {
	if err := m.database.Upsert(m.runCtx, job.records); err != nil {
		return fmt.Errorf("%w: upsert records: %w", runtime.ErrSnapshotPublish, err)
	}
	if !job.changed {
		return nil
	}
	// Reserve before Kafka: a broker-visible generation must never be reused if
	// aggregate snapshot persistence fails after publication. Retrying this same
	// plan writes the same reservation and marker, which is idempotent.
	if err := m.database.SaveGeneration(m.runCtx, job.next.Generation); err != nil {
		return fmt.Errorf("%w: reserve generation: %w", runtime.ErrSnapshotPublish, err)
	}
	messages := make([]brokers.Message, 0, len(job.upserts)+len(job.tombstones)+1)
	for _, entry := range job.upserts {
		value, err := snapshot.Marshal(entry)
		if err != nil {
			return fmt.Errorf("%w: marshal entry %q: %w", runtime.ErrSnapshotPublish, entry.Id, err)
		}
		messages = append(messages, brokers.Message{Key: []byte(entry.Id), Value: value})
	}
	for _, id := range job.tombstones {
		messages = append(messages, brokers.Message{Key: []byte(id), Value: nil})
	}
	messages = append(messages, brokers.Message{
		Key: []byte(snapshot.GenerationMarkerKey), Value: snapshot.EncodeGeneration(job.next.Generation),
	})
	if err := m.writer.WriteMessages(m.runCtx, messages...); err != nil {
		return fmt.Errorf("%w: write snapshot: %w", runtime.ErrSnapshotPublish, err)
	}
	if err := m.database.SaveSnapshot(m.runCtx, job.next); err != nil {
		return fmt.Errorf("%w: save snapshot: %w", runtime.ErrSnapshotPublish, err)
	}
	return nil
}

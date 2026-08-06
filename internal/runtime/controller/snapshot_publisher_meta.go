package controller

import (
	"context"
	"fmt"

	"ergo.services/ergo/gen"
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
	Lifecycle      SnapshotPublisherLifecycle
	Availability   runtime.Availability
	Incarnation    uint64
	RestartCount   uint64
	RestartPending bool
	Loaded         bool
	Publishing     bool
	LastError      error
}

type MessageSnapshotPublisherLoadResult struct {
	incarnation uint64
	records     []backends.ControllerRecord
	generation  int64
	snapshot    *snapshot.Snapshot
	err         error
}

type MessagePublishSnapshot struct {
	incarnation uint64
	records     []backends.ControllerRecord
	next        snapshot.Snapshot
	changed     bool
	upserts     []snapshot.EffectiveEntry
	tombstones  []string
}

type MessageSnapshotPublicationResult struct {
	incarnation uint64
	err         error
}

// snapshotPublisherMeta owns blocking persistence and broker writes for one controller incarnation.
type snapshotPublisherMeta struct {
	gen.MetaProcess

	database    backends.Database
	writer      brokers.Writer
	incarnation uint64

	runCtx context.Context
	cancel context.CancelFunc
	jobs   chan MessagePublishSnapshot
}

func (m *snapshotPublisherMeta) Init(process gen.MetaProcess) error {
	if m.database == nil || m.writer == nil {
		return fmt.Errorf("snapshot publisher meta: database and writer are required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancel = context.WithCancel(context.Background())
	m.jobs = make(chan MessagePublishSnapshot, 1)
	return nil
}

func (m *snapshotPublisherMeta) Start() error {
	records, generation, saved, err := m.load()
	if sendErr := m.Send(m.Parent(), MessageSnapshotPublisherLoadResult{
		incarnation: m.incarnation, records: records, generation: generation, snapshot: saved, err: err,
	}); sendErr != nil {
		return fmt.Errorf("%w: send result: %w", runtime.ErrSnapshotLoad, sendErr)
	}

	for {
		select {
		case <-m.runCtx.Done():
			return nil
		case job := <-m.jobs:
			err := m.publish(job)
			if sendErr := m.Send(m.Parent(), MessageSnapshotPublicationResult{
				incarnation: m.incarnation, err: err,
			}); sendErr != nil {
				return fmt.Errorf("%w: send result: %w", runtime.ErrSnapshotPublish, sendErr)
			}
		}
	}
}

func (m *snapshotPublisherMeta) HandleMessage(_ gen.PID, message any) error {
	job, ok := message.(MessagePublishSnapshot)
	if !ok || job.incarnation != m.incarnation {
		return nil
	}
	select {
	case m.jobs <- job:
		return nil
	default:
		return fmt.Errorf("%w: already queued", runtime.ErrSnapshotPublish)
	}
}

func (m *snapshotPublisherMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("snapshot publisher meta: unsupported call %T", request)
}

func (m *snapshotPublisherMeta) Terminate(error) {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *snapshotPublisherMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{"incarnation": fmt.Sprintf("%d", m.incarnation)}
}

func (m *snapshotPublisherMeta) load() ([]backends.ControllerRecord, int64, *snapshot.Snapshot, error) {
	records, err := m.database.LoadAll(m.runCtx)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%w: records: %w", runtime.ErrSnapshotLoad, err)
	}
	generation, err := m.database.LoadGeneration(m.runCtx)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%w: generation: %w", runtime.ErrSnapshotLoad, err)
	}
	saved, err := m.database.LoadSnapshot(m.runCtx)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%w: snapshot: %w", runtime.ErrSnapshotLoad, err)
	}
	return records, generation, saved, nil
}

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

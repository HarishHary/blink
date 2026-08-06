package snapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime"
)

// SnapshotReaderMetaLifecycle describes one blocking broker-reader
// meta-process incarnation.
type SnapshotReaderMetaLifecycle string

const (
	SnapshotReaderMetaStarting   SnapshotReaderMetaLifecycle = "starting"
	SnapshotReaderMetaRunning    SnapshotReaderMetaLifecycle = "running"
	SnapshotReaderMetaRestarting SnapshotReaderMetaLifecycle = "restarting"
	SnapshotReaderMetaStopped    SnapshotReaderMetaLifecycle = "stopped"
)

// SnapshotReaderMetaStatus is owned by snapshotReaderActor because that actor
// owns the meta-process incarnation, restart policy, and interpretation of
// MessageDownAlias. The meta-process only reports runtime facts.
type SnapshotReaderMetaStatus struct {
	Lifecycle      SnapshotReaderMetaLifecycle
	Availability   runtime.Availability
	Incarnation    uint64
	RestartCount   uint64
	CaughtUp       bool
	RestartPending bool
	LastError      error
}

const snapshotCatchUpPoll = 250 * time.Millisecond

// Reader meta messages are fenced by incarnation in the parent actor.
type MessageSnapshotReaderStarted struct{ incarnation uint64 }

type MessageSnapshotRecord struct {
	incarnation uint64
	message     brokers.Message
}

type MessageSnapshotCaughtUp struct{ incarnation uint64 }

// snapshotReaderMeta owns one concrete broker reader and one blocking read
// loop. Its parent actor owns reader creation, restart/backoff, public status,
// snapshot state, and publication.
type snapshotReaderMeta struct {
	gen.MetaProcess

	reader      brokers.Reader
	incarnation uint64

	runCtx    context.Context
	cancelRun context.CancelFunc
}

func (m *snapshotReaderMeta) Init(process gen.MetaProcess) error {
	if m.reader == nil {
		return fmt.Errorf("snapshot reader meta: reader is required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	return nil
}

func (m *snapshotReaderMeta) Start() error {
	if err := m.Send(m.Parent(), MessageSnapshotReaderStarted{incarnation: m.incarnation}); err != nil {
		return fmt.Errorf("%w: announce start: %w", runtime.ErrSnapshotRead, err)
	}

	caughtUp := false
	for {
		readCtx := m.runCtx
		cancel := func() {}
		if !caughtUp {
			readCtx, cancel = context.WithTimeout(m.runCtx, snapshotCatchUpPoll)
		}

		message, err := m.reader.ReadMessage(readCtx)
		cancel()

		if err != nil {
			if m.runCtx.Err() != nil {
				return nil
			}
			if !caughtUp && errors.Is(err, context.DeadlineExceeded) {
				lag, lagErr := m.reader.ReadLag(m.runCtx)
				if lagErr != nil {
					return fmt.Errorf("%w: read lag: %w", runtime.ErrSnapshotRead, lagErr)
				}
				if lag == 0 {
					caughtUp = true
					if sendErr := m.Send(m.Parent(), MessageSnapshotCaughtUp{incarnation: m.incarnation}); sendErr != nil {
						return fmt.Errorf("%w: send caught-up: %w", runtime.ErrSnapshotRead, sendErr)
					}
				}
				continue
			}
			return fmt.Errorf("%w: read message: %w", runtime.ErrSnapshotRead, err)
		}

		message.Key = append([]byte(nil), message.Key...)
		message.Value = append([]byte(nil), message.Value...)
		if err := m.Send(m.Parent(), MessageSnapshotRecord{
			incarnation: m.incarnation,
			message:     message,
		}); err != nil {
			return fmt.Errorf("%w: send record: %w", runtime.ErrSnapshotRead, err)
		}
	}
}

func (m *snapshotReaderMeta) HandleMessage(gen.PID, any) error { return nil }

func (m *snapshotReaderMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("snapshot reader meta: unsupported call %T", request)
}

func (m *snapshotReaderMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
	if m.reader != nil {
		_ = m.reader.Close()
	}
}

func (m *snapshotReaderMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{"incarnation": fmt.Sprintf("%d", m.incarnation)}
}

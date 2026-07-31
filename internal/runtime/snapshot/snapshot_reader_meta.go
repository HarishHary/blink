package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
)

const snapshotCatchUpPoll = 250 * time.Millisecond

// snapshotReaderMetaStarted, snapshotReaderMetaRecord, and
// snapshotReaderMetaCaughtUp are emitted by one reader incarnation. The parent
// actor fences every message by generation.
type snapshotReaderMetaStarted struct{ generation uint64 }

type snapshotReaderMetaRecord struct {
	generation uint64
	message    brokers.Message
}

type snapshotReaderMetaCaughtUp struct{ generation uint64 }

// snapshotReaderMeta owns one concrete broker reader and one blocking read
// loop. Its parent actor owns reader creation, restart/backoff, public status,
// snapshot state, and publication.
type snapshotReaderMeta struct {
	gen.MetaProcess

	reader     brokers.Reader
	generation uint64

	runCtx    context.Context
	cancelRun context.CancelFunc
	closeOnce sync.Once
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
	if err := m.Send(m.Parent(), snapshotReaderMetaStarted{generation: m.generation}); err != nil {
		return fmt.Errorf("snapshot reader meta: announce start: %w", err)
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
					return fmt.Errorf("snapshot reader meta: read lag: %w", lagErr)
				}
				if lag == 0 {
					caughtUp = true
					if sendErr := m.Send(m.Parent(), snapshotReaderMetaCaughtUp{generation: m.generation}); sendErr != nil {
						return fmt.Errorf("snapshot reader meta: send caught-up: %w", sendErr)
					}
				}
				continue
			}
			return fmt.Errorf("snapshot reader meta: read message: %w", err)
		}

		if err := m.Send(m.Parent(), snapshotReaderMetaRecord{
			generation: m.generation,
			message:    message,
		}); err != nil {
			return fmt.Errorf("snapshot reader meta: send record: %w", err)
		}
	}
}

func (m *snapshotReaderMeta) HandleMessage(gen.PID, any) error { return nil }

func (m *snapshotReaderMeta) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (m *snapshotReaderMeta) Terminate(error) {
	m.closeOnce.Do(func() {
		if m.cancelRun != nil {
			m.cancelRun()
		}
		if m.reader != nil {
			_ = m.reader.Close()
		}
	})
}

func (m *snapshotReaderMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{"generation": fmt.Sprintf("%d", m.generation)}
}

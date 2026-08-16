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

const catchUpPoll = 250 * time.Millisecond

// ReaderMetaLifecycle describes one blocking broker-reader
// meta-process instance.
type ReaderMetaLifecycle string

const (
	ReaderMetaStarting   ReaderMetaLifecycle = "starting"
	ReaderMetaRunning    ReaderMetaLifecycle = "running"
	ReaderMetaRestarting ReaderMetaLifecycle = "restarting"
	ReaderMetaStopped    ReaderMetaLifecycle = "stopped"
)

// readerMetaStatus is owned by readerActor because that actor
// owns the meta-process restart policy and interpretation of MessageDownAlias.
// The meta-process only reports runtime facts.
type readerMetaStatus struct {
	Lifecycle    ReaderMetaLifecycle
	Availability runtime.Availability
	CaughtUp     bool
	LastError    error
}

// readerMeta owns one concrete broker reader and one blocking read
// loop. Its parent actor owns reader creation, restart/backoff, public status,
// snapshot state, and publication.
type readerMeta struct {
	gen.MetaProcess
	reader    brokers.Reader
	runCtx    context.Context
	cancelRun context.CancelFunc
}

// --- messages ---

// Reader meta messages are fenced by source alias in the parent actor.
type MessageRecord struct {
	source  gen.Alias
	message brokers.Message
}
type MessageCaughtUp struct{ source gen.Alias }

// --- messages ---

// Init validates the reader and initializes its cancellation context.
func (m *readerMeta) Init(process gen.MetaProcess) error {
	if m.reader == nil {
		return fmt.Errorf("snapshot reader meta: reader is required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	return nil
}

// Start reads broker records and reports them to the parent actor.
func (m *readerMeta) Start() error {
	caughtUp := false
	for {
		readCtx := m.runCtx
		cancel := func() {}
		if !caughtUp {
			readCtx, cancel = context.WithTimeout(m.runCtx, catchUpPoll)
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
					if sendErr := m.Send(m.Parent(), MessageCaughtUp{source: m.ID()}); sendErr != nil {
						return fmt.Errorf("%w: send caught-up: %w", runtime.ErrSnapshotRead, sendErr)
					}
				}
				continue
			}
			return fmt.Errorf("%w: read message: %w", runtime.ErrSnapshotRead, err)
		}

		message.Key = append([]byte(nil), message.Key...)
		message.Value = append([]byte(nil), message.Value...)
		if err := m.Send(m.Parent(), MessageRecord{
			source:  m.ID(),
			message: message,
		}); err != nil {
			return fmt.Errorf("%w: send record: %w", runtime.ErrSnapshotRead, err)
		}
	}
}

// HandleMessage ignores asynchronous messages.
func (m *readerMeta) HandleMessage(gen.PID, any) error { return nil }

// HandleCall rejects unsupported synchronous requests.
func (m *readerMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("snapshot reader meta: unsupported call %T", request), nil
}

// Terminate cancels reading and closes the broker reader.
func (m *readerMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
	if m.reader != nil {
		_ = m.reader.Close()
	}
}

// HandleInspect returns no inspection data.
func (m *readerMeta) HandleInspect(gen.PID, ...string) map[string]string { return nil }

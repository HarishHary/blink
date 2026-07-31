package runtime

import (
	"context"
	"sync"
)

// asyncResult is the one-shot completion channel shared by Runtime and
// runtimeSupervisor for one submitted invocation. Completion is idempotent so
// send failures, child termination, cancellation, and normal completion may
// race without blocking or double-completing the caller.
type AsyncResult struct {
	Once sync.Once
	Ch   chan error
}

func NewAsyncResult() *AsyncResult {
	return &AsyncResult{Ch: make(chan error, 1)}
}

func (r *AsyncResult) Complete(err error) {
	if r == nil {
		return
	}
	r.Once.Do(func() {
		r.Ch <- err
		close(r.Ch)
	})
}

// Invocation is the asynchronous handle returned by Runtime.Submit and Runtime.SubmitShadow.
type Invocation struct {
	Id    uint64
	State *invocationState
}

// Done is closed after the invocation reaches one terminal result.
func (i Invocation) Done() <-chan struct{} {
	if i.State == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return i.State.done
}

// Err returns the terminal invocation error. It returns nil before completion
// and for a successful invocation.
func (i Invocation) Err() error {
	if i.State == nil {
		return nil
	}
	i.State.mu.RLock()
	defer i.State.mu.RUnlock()
	return i.State.err
}

// Cancel requests cancellation of this invocation. Cancellation is idempotent;
// the invocation completes only when the runtime acknowledges a terminal
// result through the normal completion path.
func (i Invocation) Cancel(err error) {
	if i.State == nil {
		return
	}
	i.State.RequestCancel(err)
}

type invocationState struct {
	done chan struct{}

	mu  sync.RWMutex
	err error

	completeOnce sync.Once
	cancelOnce   sync.Once
	cancel       func(error)
}

func NewInvocationState(cancel func(error)) *invocationState {
	if cancel == nil {
		cancel = func(error) {}
	}
	return &invocationState{
		done:   make(chan struct{}),
		cancel: cancel,
	}
}

func (s *invocationState) RequestCancel(err error) {
	if s == nil {
		return
	}
	if err == nil {
		err = context.Canceled
	}
	s.cancelOnce.Do(func() {
		s.cancel(err)
	})
}

func (s *invocationState) Complete(err error) {
	if s == nil {
		return
	}
	s.completeOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

package runtime

import (
	"context"
	"sync/atomic"
)

const ioBarrierSealed = uint64(1) << 63

// IOBarrier prevents shutdown-owned resources from closing while a caller may
// still use them. Create one per resource lifecycle and share it across every
// actor or meta using those resources. Construct it with NewIOBarrier; the zero
// value is not usable. A sealed barrier cannot be reused.
type IOBarrier struct {
	state    atomic.Uint64
	quiesced chan struct{}
}

// NewIOBarrier creates an externally owned, unsealed I/O barrier.
func NewIOBarrier() *IOBarrier {
	return &IOBarrier{quiesced: make(chan struct{})}
}

// Acquire reserves I/O unless the barrier is sealed. Each success requires one Release.
func (b *IOBarrier) Acquire() bool {
	for {
		state := b.state.Load()
		if state&ioBarrierSealed != 0 || state == ioBarrierSealed-1 {
			return false
		}
		if b.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

// Release frees one I/O reservation after its cleanup has finished.
func (b *IOBarrier) Release() {
	for {
		state := b.state.Load()
		count := state &^ ioBarrierSealed
		if count == 0 {
			return
		}
		next := state - 1
		if !b.state.CompareAndSwap(state, next) {
			continue
		}
		if next == ioBarrierSealed {
			close(b.quiesced)
		}
		return
	}
}

// Seal rejects new I/O reservations. Call it from shutdown orchestration, then
// use a bounded context with WaitQuiesced; never wait from an actor callback.
func (b *IOBarrier) Seal() {
	for {
		state := b.state.Load()
		if state&ioBarrierSealed != 0 {
			return
		}
		next := state | ioBarrierSealed
		if !b.state.CompareAndSwap(state, next) {
			continue
		}
		if state == 0 {
			close(b.quiesced)
		}
		return
	}
}

// WaitQuiesced waits until Seal was called and every accepted I/O operation
// completed its cleanup. A timeout does not establish quiescence: resources
// must remain open while I/O is outstanding.
func (b *IOBarrier) WaitQuiesced(ctx context.Context) error {
	select {
	case <-b.quiesced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Quiesced reports whether the barrier is sealed and all accepted I/O has completed cleanup.
func (b *IOBarrier) Quiesced() bool {
	return b.state.Load() == ioBarrierSealed
}

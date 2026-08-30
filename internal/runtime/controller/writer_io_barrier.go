package controller

import (
	"context"
	"sync/atomic"
)

const writerIOBarrierSealed = uint64(1) << 63

// writerIOBarrier prevents resource closure while a writer meta may use it.
// Its count is meaningful only after Seal: no later Acquire can then succeed.
type writerIOBarrier struct {
	state    atomic.Uint64
	quiesced chan struct{}
}

// newWriterIOBarrier creates an unsealed writer I/O barrier.
func newWriterIOBarrier() *writerIOBarrier {
	return &writerIOBarrier{quiesced: make(chan struct{})}
}

// Acquire reserves writer I/O unless the barrier is sealed.
func (b *writerIOBarrier) Acquire() bool {
	for {
		state := b.state.Load()
		if state&writerIOBarrierSealed != 0 {
			return false
		}
		if state == writerIOBarrierSealed-1 {
			return false
		}
		if b.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

// Release frees one writer I/O reservation.
func (b *writerIOBarrier) Release() {
	for {
		state := b.state.Load()
		count := state &^ writerIOBarrierSealed
		if count == 0 {
			return
		}
		next := state - 1
		if !b.state.CompareAndSwap(state, next) {
			continue
		}
		if next == writerIOBarrierSealed {
			close(b.quiesced)
		}
		return
	}
}

// Seal blocks new reservations and signals when existing I/O ends.
func (b *writerIOBarrier) Seal() {
	for {
		state := b.state.Load()
		if state&writerIOBarrierSealed != 0 {
			return
		}
		next := state | writerIOBarrierSealed
		if !b.state.CompareAndSwap(state, next) {
			continue
		}
		if state == 0 {
			close(b.quiesced)
		}
		return
	}
}

// WaitQuiesced waits for the sealed barrier to become idle.
func (b *writerIOBarrier) WaitQuiesced(ctx context.Context) error {
	select {
	case <-b.quiesced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Quiesced reports whether sealing and all reserved I/O have completed.
func (b *writerIOBarrier) Quiesced() bool {
	return b.state.Load() == writerIOBarrierSealed
}

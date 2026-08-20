package controller

import (
	"context"
	"sync/atomic"
)

const publisherIOBarrierSealed = uint64(1) << 63

// publisherIOBarrier prevents resource closure while a publisher meta may use it.
// Its count is meaningful only after Seal: no later Acquire can then succeed.
type publisherIOBarrier struct {
	state    atomic.Uint64
	quiesced chan struct{}
}

// newPublisherIOBarrier creates an unsealed publisher I/O barrier.
func newPublisherIOBarrier() *publisherIOBarrier {
	return &publisherIOBarrier{quiesced: make(chan struct{})}
}

// Acquire reserves publisher I/O unless the barrier is sealed.
func (b *publisherIOBarrier) Acquire() bool {
	for {
		state := b.state.Load()
		if state&publisherIOBarrierSealed != 0 {
			return false
		}
		if state == publisherIOBarrierSealed-1 {
			return false
		}
		if b.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

// Release frees one publisher I/O reservation.
func (b *publisherIOBarrier) Release() {
	for {
		state := b.state.Load()
		count := state &^ publisherIOBarrierSealed
		if count == 0 {
			return
		}
		next := state - 1
		if !b.state.CompareAndSwap(state, next) {
			continue
		}
		if next == publisherIOBarrierSealed {
			close(b.quiesced)
		}
		return
	}
}

// Seal blocks new reservations and signals when existing I/O ends.
func (b *publisherIOBarrier) Seal() {
	for {
		state := b.state.Load()
		if state&publisherIOBarrierSealed != 0 {
			return
		}
		next := state | publisherIOBarrierSealed
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
func (b *publisherIOBarrier) WaitQuiesced(ctx context.Context) error {
	select {
	case <-b.quiesced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Quiesced reports whether sealing and all reserved I/O have completed.
func (b *publisherIOBarrier) Quiesced() bool {
	return b.state.Load() == publisherIOBarrierSealed
}

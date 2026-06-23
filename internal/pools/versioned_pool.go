package pools

import (
	"context"
	"fmt"
	"sync/atomic"
)

// PoolKey uniquely identifies a plugin subprocess pool.
// Id is the logical plugin UUID from the YAML sidecar.
// Name is the binary stem (= YAML stem), unique per directory.
// Hash is the SHA-256 of the binary, changes on every binary update.
type PoolKey struct {
	Id   string
	Name string
	Hash string
}

func (k PoolKey) String() string {
	if k.Hash != "" {
		return k.Id + "@" + k.Name + "@" + k.Hash
	}
	return k.Id + "@" + k.Name
}

// VersionedPool manages a fixed-size pool of plugin subprocess handles of type T.
// Acquire/Release use a channel-based semaphore; handles are stateful gRPC connections
// and must not be discarded by the GC (no sync.Pool).
type VersionedPool[T any] struct {
	key      PoolKey
	slots    chan T
	inflight atomic.Int64
	draining atomic.Bool
}

func newVersionedPool[T any](key PoolKey, plugins []T, maxProcs int) *VersionedPool[T] {
	size := maxProcs
	if size < len(plugins) {
		size = len(plugins)
	}
	p := &VersionedPool[T]{
		key:   key,
		slots: make(chan T, size),
	}
	for _, plugin := range plugins {
		p.slots <- plugin
	}
	return p
}

// Returns a plugin handle for exclusive use. Blocks until one is available or ctx is cancelled.
// Returns an error if the pool is draining.
func (p *VersionedPool[T]) Acquire(ctx context.Context) (T, error) {
	if p.draining.Load() {
		var zero T
		return zero, fmt.Errorf("pool %s is draining", p.key)
	}
	select {
	case plugin := <-p.slots:
		p.inflight.Add(1)
		return plugin, nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Release returns the handle to the pool after use.
func (p *VersionedPool[T]) Release(plugin T) {
	p.inflight.Add(-1)
	p.slots <- plugin
}

// Inflight returns the number of calls currently executing in this pool.
func (p *VersionedPool[T]) Inflight() int64 {
	return p.inflight.Load()
}

// Size returns the total capacity of this pool.
func (p *VersionedPool[T]) Size() int {
	return cap(p.slots)
}

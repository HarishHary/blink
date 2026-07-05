package pools

import (
	"context"
	"fmt"
	"sync/atomic"
)

// PoolKey uniquely identifies a plugin subprocess pool: Id (logical UUID from YAML), Name (binary stem), Hash (SHA-256 of the binary, changes every build).
type PoolKey struct {
	Id   string
	Name string
	Hash string
}

// String renders the key as Id@Name@Hash (Hash omitted when empty).
func (k PoolKey) String() string {
	if k.Hash != "" {
		return k.Id + "@" + k.Name + "@" + k.Hash
	}
	return k.Id + "@" + k.Name
}

// VersionedPool is a fixed-size channel semaphore over one binary version's subprocess handles; they're stateful gRPC conns, so no sync.Pool.
type VersionedPool[T any] struct {
	key      PoolKey
	slots    chan T
	inflight atomic.Int64
	draining atomic.Bool
}

// newVersionedPool creates a pool seeded with plugins; capacity is max(maxProcs, len(plugins)).
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

// Acquire returns a handle for exclusive use, blocking until one is free or ctx is cancelled; errors if the pool is draining.
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

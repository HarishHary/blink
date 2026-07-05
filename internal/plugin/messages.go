package plugin

import (
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/pools"
)

// Notify is the callback the executor calls on plugin start/update/stop (typically pool.Sync, which registers/deregisters handles).
type Notify = func(messaging.Message)

// RegisterMessage is delivered when a new subprocess is ready; Items holds all N workers, MaxProcs is the pool capacity hint.
type RegisterMessage[T Syncable] struct {
	messaging.IsMessage
	Items    []T
	MaxProcs int
}

// UnregisterMessage is delivered on a transient stop (crash-restart or config-disable); the pool drops the versioned pool but does NOT tombstone the ID.
type UnregisterMessage[T Syncable] struct {
	messaging.IsMessage
	ItemKey pools.PoolKey
}

// RemoveMessage is delivered when a binary is permanently deleted; the pool drops the versioned pool and tombstones the ID if no other pools remain.
type RemoveMessage[T Syncable] struct {
	messaging.IsMessage
	ItemKey pools.PoolKey
}

// UpdateMessage is delivered when a binary changes in place; Items holds the new N workers, and OnDrained (fired by ProcessPool.drain once in-flight calls finish) kills the old subprocesses.
type UpdateMessage[T Syncable] struct {
	messaging.IsMessage
	Items     []T
	MaxProcs  int
	OnDrained func()
}

func NewRegisterMessage[T Syncable](items []T, maxProcs int) RegisterMessage[T] {
	return RegisterMessage[T]{Items: items, MaxProcs: maxProcs}
}

func NewUnregisterMessage[T Syncable](key pools.PoolKey) UnregisterMessage[T] {
	return UnregisterMessage[T]{ItemKey: key}
}

func NewRemoveMessage[T Syncable](key pools.PoolKey) RemoveMessage[T] {
	return RemoveMessage[T]{ItemKey: key}
}

func NewUpdateMessage[T Syncable](items []T, maxProcs int, onDrained func()) UpdateMessage[T] {
	return UpdateMessage[T]{Items: items, MaxProcs: maxProcs, OnDrained: onDrained}
}

// MigrateMessage is delivered when two binaries for one ID swap modes (YAML-only, e.g. canary promoted to stable); no process is killed/spawned, only rollout slots are reassigned.
type MigrateMessage[T Syncable] struct {
	messaging.IsMessage
	ActiveKey  pools.PoolKey // key that should hold active[id]
	PendingKey pools.PoolKey // key that should hold pending[id] (zero = clear)
}

func NewMigrateMessage[T Syncable](activeKey, pendingKey pools.PoolKey) MigrateMessage[T] {
	return MigrateMessage[T]{ActiveKey: activeKey, PendingKey: pendingKey}
}

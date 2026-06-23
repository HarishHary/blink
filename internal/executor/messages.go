package executor

import (
	"github.com/harishhary/blink/internal/messaging"
	internal "github.com/harishhary/blink/internal/pools"
)

// Notify is the callback a PluginManager calls when a plugin starts, updates, or stops.
// Implementations are typically pool.Sync methods that register/deregister plugin handles.
type Notify = func(messaging.Message)

// Delivered when a new plugin subprocess is ready.
// Items holds all N worker instances for the binary; MaxProcs is the pool capacity hint.
type RegisterMessage[T Syncable] struct {
	messaging.IsMessage
	Items    []T
	MaxProcs int
}

// Delivered when a plugin subprocess is stopped transiently aka a crash being restarted, or a plugin disabled via config. The plugin may come back.
// Pool removes the specific versioned pool but does NOT tombstone the plugin ID.
type UnregisterMessage[T Syncable] struct {
	messaging.IsMessage
	ItemKey internal.PoolKey
}

// Delivered when a plugin binary is permanently deleted from disk.
// The plugin is not expected to return. Pool removes the specific versioned pool and tombstones the plugin ID only if no other pools remain.
type RemoveMessage[T Syncable] struct {
	messaging.IsMessage
	ItemKey internal.PoolKey
}

// Delivered when a plugin binary changes in-place.
// Items holds all N worker instances for the new binary version.
// OnDrained is called by ProcessPool.drain once all in-flight calls on the old VersionedPool complete - the PluginManager uses it to kill the old subprocesses only after the pool has finished draining.
type UpdateMessage[T Syncable] struct {
	messaging.IsMessage
	Items     []T
	MaxProcs  int
	OnDrained func()
}

func NewRegisterMessage[T Syncable](items []T, maxProcs int) RegisterMessage[T] {
	return RegisterMessage[T]{Items: items, MaxProcs: maxProcs}
}

func NewUnregisterMessage[T Syncable](key internal.PoolKey) UnregisterMessage[T] {
	return UnregisterMessage[T]{ItemKey: key}
}

func NewRemoveMessage[T Syncable](key internal.PoolKey) RemoveMessage[T] {
	return RemoveMessage[T]{ItemKey: key}
}

func NewUpdateMessage[T Syncable](items []T, maxProcs int, onDrained func()) UpdateMessage[T] {
	return UpdateMessage[T]{Items: items, MaxProcs: maxProcs, OnDrained: onDrained}
}

// Delivered when two binaries for the same plugin ID swap modes via a YAML-only change
// (e.g. the canary is promoted to stable while the old stable becomes the new canary).
// No process is killed or spawned; only the pool routing slots are reassigned.
type MigrateMessage[T Syncable] struct {
	messaging.IsMessage
	ActiveKey  internal.PoolKey // key that should hold active[id]
	PendingKey internal.PoolKey // key that should hold pending[id] (zero = clear)
}

func NewMigrateMessage[T Syncable](activeKey, pendingKey internal.PoolKey) MigrateMessage[T] {
	return MigrateMessage[T]{ActiveKey: activeKey, PendingKey: pendingKey}
}

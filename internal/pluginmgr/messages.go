package pluginmgr

import "github.com/harishhary/blink/internal/messaging"

// Notify is the callback a PluginManager calls when a plugin starts, updates, or stops.
// Implementations are typically pool.Sync methods that register/deregister plugin handles.
type Notify = func(messaging.Message)

// Delivered when a new plugin subprocess is ready.
// Items holds all N worker instances for the binary; MaxProcs is the pool capacity hint.
type RegisterMessage[T ISyncable] struct {
	messaging.IsMessage
	Items    []T
	MaxProcs int
}

// Delivered when a plugin subprocess is stopped transiently aka a crash being restarted, or a plugin disabled via config. The plugin may come back.
// Pool removes the active entry but does NOT tombstone the plugin ID.
type UnregisterMessage[T ISyncable] struct {
	messaging.IsMessage
	ItemID string
}

// Delivered when a plugin binary is permanently deleted from disk.
// The plugin is not expected to return. Pool removes the active entry AND tombstones the plugin ID.
type RemoveMessage[T ISyncable] struct {
	messaging.IsMessage
	ItemID string
}

// Delivered when a plugin binary changes in-place.
// Items holds all N worker instances for the new binary version.
// OnDrained is called by ProcessPool.drain once all in-flight calls on the old VersionedPool complete - the PluginManager uses it to kill the old subprocesses only after the pool has finished draining.
type UpdateMessage[T ISyncable] struct {
	messaging.IsMessage
	Items     []T
	MaxProcs  int
	OnDrained func()
}

func NewRegisterMessage[T ISyncable](items []T, maxProcs int) RegisterMessage[T] {
	return RegisterMessage[T]{Items: items, MaxProcs: maxProcs}
}

func NewUnregisterMessage[T ISyncable](itemID string) UnregisterMessage[T] {
	return UnregisterMessage[T]{ItemID: itemID}
}

func NewRemoveMessage[T ISyncable](itemID string) RemoveMessage[T] {
	return RemoveMessage[T]{ItemID: itemID}
}

func NewUpdateMessage[T ISyncable](items []T, maxProcs int, onDrained func()) UpdateMessage[T] {
	return UpdateMessage[T]{Items: items, MaxProcs: maxProcs, OnDrained: onDrained}
}

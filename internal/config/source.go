package config

import (
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
)

// Source[T] is the read surface the data plane consumes for one plugin type (satisfied by
// SnapshotConfig[T]); rpc wrappers, pools, and adapters depend on this interface, not a concrete config type.
type Source[T plugin.Syncable] interface {
	// DesiredConfig gives the executor its lifecycle view (DesiredBinaryState).
	plugin.DesiredConfig

	// ByFileName returns the metadata for one binary stem (primary or candidate), for rpc wrappers.
	ByFileName(name string) (T, bool)
	// RoutingByID returns the merged rollout routing for a logical plugin ID.
	RoutingByID(id string) pools.RoutingEntry
	// Primaries returns the effective (stable) items, for consumers enumerating the active catalog (e.g. event_matcher).
	Primaries() []T
}

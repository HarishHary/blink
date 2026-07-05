package config

import (
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
)

// Source[T] is the data plane's read surface for one plugin type (satisfied by SnapshotConfig[T]).
type Source[T plugin.Syncable] interface {
	// DesiredConfig gives the executor its lifecycle view (DesiredBinaryState).
	plugin.DesiredConfig

	// ByFileName returns the metadata for one binary stem (primary or candidate), for rpc wrappers.
	ByFileName(name string) (T, bool)
	// RolloutById returns the merged rollout for a logical plugin ID.
	RolloutById(id string) pools.RolloutEntry
	// Primaries returns the effective (stable) items, for consumers enumerating the active catalog (e.g. event_matcher).
	Primaries() []T
}

// RolloutFor adapts a Source[T] into the pools.RolloutConfig a pool constructor needs (see RolloutConfig).
func RolloutFor[T plugin.Syncable](src Source[T]) pools.RolloutConfig {
	return func(id, name string) (pools.RolloutMode, float64) {
		if name != "" {
			if m, ok := src.ByFileName(name); ok {
				md := m.Metadata()
				return md.RolloutMode, md.RolloutPct
			}
		}
		re := src.RolloutById(id)
		return re.RolloutMode, re.RolloutPct
	}
}

package plugin

import "github.com/harishhary/blink/internal/pools"

// BinaryState is the normalized desired state for one binary; PluginAdapter uses it to implement the shared adapter methods without per-type config dependencies.
type BinaryState struct {
	Id         string
	Name       string
	Enabled    bool
	Mode       pools.RolloutMode
	RolloutPct float64
	MaxProcs   int
}

// DesiredConfig is the read-only interface PluginAdapter queries for a binary's desired config; satisfied by the type's snapshot-backed SnapshotConfig.
type DesiredConfig interface {
	// DesiredBinaryState returns the desired state keyed by binary filename (no extension).
	// Returns false when the snapshot names no artifact for the binary (or its spec is unparseable).
	DesiredBinaryState(name string) (BinaryState, bool)
}

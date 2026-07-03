// Package snapshot holds the control-plane wire model - the effective desired state the
// controller computes and publishes, and that executors consume. It is a deliberate leaf
// package (imports only internal/pools) so the controller, its DB backends, and the
// executors can all depend on the Snapshot type without anyone importing the heavy
// controller package.
//
// Do NOT import internal/controller here: controller depends on backends and plugin, both
// of which depend on this package; folding these types into controller would form import
// cycles (controller↔backends and controller↔plugin).
//
// Persistence/history types (ControllerRecord, RecordStatus) live in internal/backends -
// this package is purely the wire model.
package snapshot

import "github.com/harishhary/blink/internal/pools"

// ArtifactRef is one binary artifact for a logical plugin ID.
type ArtifactRef struct {
	Name string            // binary filename (== YAML sidecar stem)
	Mode pools.RolloutMode // rollout mode declared in its YAML sidecar

	// Spec is this artifact's yaml-marshaled plugin metadata. It lets a consumer reconstruct
	// the full plugin config from the snapshot alone - e.g. event_matcher routes off the rule
	// snapshot's specs with no RULE_CONFIG_DIR, and rule_executor sources its whole rule
	// catalog (including the canary candidate) from the snapshot. Empty when unmarshalable.
	Spec []byte

	// Hash is the sha256 (hex) of the artifact's binary as the controller saw it. It makes a
	// binary change flow through the snapshot (bumping the generation) and lets a consumer verify
	// the binary it resolves locally matches what the control plane published. Empty when the
	// controller saw no binary for this artifact.
	Hash string
}

// EffectiveEntry is the computed desired state for one logical plugin ID.
// Primary is the BG (active) artifact; Candidate is the CN/SH artifact if present.
type EffectiveEntry struct {
	Id        string
	Enabled   bool         // true if at least one artifact is enabled
	Primary   *ArtifactRef // nil when the ID has no stable BG artifact yet
	Candidate *ArtifactRef // nil when the ID is BG-only
}

// Snapshot is a computed desired state (a sorted set of EffectiveEntry) consumed by executors.
//
// Generation's meaning depends on which producer stamped it:
//   - The PluginController writes its persisted DB generation, bumped when the effective content
//     changes and once per controller restart (bootstrap treats the empty prior snapshot as a
//     change). Durable, monotonic, and fleet-wide - the number Option B tracks across pods.
//   - LocalReader/SnapshotReader write a per-pod localRevision - an in-memory counter that resets
//     on restart and differs between pods. It exists only as a change token so SnapshotConfig knows
//     when to re-parse specs; it is NOT comparable across pods or to the DB generation.
type Snapshot struct {
	Generation int64
	Entries    []EffectiveEntry
}

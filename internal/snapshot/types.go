// Package snapshot holds the control-plane wire model: the effective desired state the controller
// publishes and executors consume. Keep it a leaf (import only internal/pools) - importing
// internal/controller would form an import cycle.
package snapshot

import "github.com/harishhary/blink/internal/pools"

// ArtifactRef is one binary artifact for a logical plugin ID.
type ArtifactRef struct {
	Name        string            // binary filename (== YAML sidecar stem)
	RolloutMode pools.RolloutMode // from its YAML sidecar

	// Spec is this artifact's yaml-marshaled plugin metadata, letting a consumer reconstruct the
	// full plugin config from the snapshot alone (no *_CONFIG_DIR). Empty when unmarshalable.
	Spec []byte

	// Hash is the sha256 (hex) of the artifact's binary as the controller saw it: flows binary
	// changes through the snapshot and lets a consumer verify its local binary. Empty if none.
	Hash string
}

// Clone returns a deep copy of the artifact reference.
func (r *ArtifactRef) Clone() *ArtifactRef {
	if r == nil {
		return nil
	}
	clone := *r
	clone.Spec = append([]byte(nil), r.Spec...)
	return &clone
}

// EffectiveEntry is the computed desired state for one logical plugin ID.
// Primary is the BG (active) artifact; Candidate is the CN/SH artifact if present.
type EffectiveEntry struct {
	Id        string
	Enabled   bool         // true if at least one artifact is enabled
	Primary   *ArtifactRef // nil when the ID has no stable BG artifact yet
	Candidate *ArtifactRef // nil when the ID is BG-only
}

// Clone returns a deep copy of the effective entry.
func (e EffectiveEntry) Clone() EffectiveEntry {
	e.Primary = e.Primary.Clone()
	e.Candidate = e.Candidate.Clone()
	return e
}

// Snapshot is a computed desired state (a sorted set of EffectiveEntry) consumed by executors.
// Generation is either the controller's durable fleet-wide DB generation or a reader's per-pod
// localRevision change-token - the two are not comparable across pods.
type Snapshot struct {
	Generation int64
	Entries    []EffectiveEntry
}

// Clone returns a deep copy of the snapshot.
func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return nil
	}
	clone := *s
	clone.Entries = append([]EffectiveEntry(nil), s.Entries...)
	for i := range clone.Entries {
		clone.Entries[i] = clone.Entries[i].Clone()
	}
	return &clone
}

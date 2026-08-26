// Package snapshot holds the control-plane wire model: the effective desired state the controller
// publishes and executors consume. Keep it a leaf - importing
// internal/controller would form an import cycle.
package snapshot

import (
	"github.com/harishhary/blink/internal/runtime"
)

// ArtifactRef is one binary artifact for a logical plugin ID.
type ArtifactRef struct {
	Name        string              // binary filename (== YAML sidecar stem)
	RolloutMode runtime.RolloutMode // from its YAML sidecar

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

// ArtifactRefEqual reports whether two artifact references match.
func ArtifactRefEqual(left, right *ArtifactRef) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if left == nil {
		return true
	}
	return left.Name == right.Name && left.RolloutMode == right.RolloutMode && left.Hash == right.Hash && string(left.Spec) == string(right.Spec)
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

// CloneEntries returns independent copies of catalog entries.
func CloneEntries(entries []EffectiveEntry) []EffectiveEntry {
	cloned := append([]EffectiveEntry(nil), entries...)
	for i := range cloned {
		cloned[i] = cloned[i].Clone()
	}
	return cloned
}

// EffectiveEntryEqual reports whether two entries have identical routing data.
func EffectiveEntryEqual(left, right EffectiveEntry) bool {
	return left.Id == right.Id && left.Enabled == right.Enabled &&
		ArtifactRefEqual(left.Primary, right.Primary) && ArtifactRefEqual(left.Candidate, right.Candidate)
}

// ChangeKind classifies why an upserted entry differs from what was previously committed. Computed
// by internal/runtime/controller.ClassifyChanges (which needs the namespace's plugin.Loader[T] to
// isolate a rollout-percentage-only change), but the value type itself lives here so it can travel
// on SnapshotUpdate without controller depending back on this package's dependents.
type ChangeKind uint8

const (
	ChangeAdded ChangeKind = iota
	ChangeUpdated
	ChangeRolloutMode
	ChangeTrafficSplit
)

// EntryChange pairs one upserted entry with why it changed from the prior commit.
type EntryChange struct {
	Kind  ChangeKind
	Entry EffectiveEntry
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

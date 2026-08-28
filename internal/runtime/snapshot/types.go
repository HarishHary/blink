package snapshot

import (
	"github.com/harishhary/blink/internal/runtime"
)

// ArtifactRef is one binary artifact for a logical plugin ID: its filename, its sidecar's rollout
// mode, its yaml-marshaled spec, and the sha256 of the binary as the controller saw it.
type ArtifactRef struct {
	Name        string
	RolloutMode runtime.RolloutMode
	Spec        []byte
	Hash        string
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

// EffectiveEntry is the computed desired state for one logical plugin ID: Primary is the active BG
// artifact, Candidate the CN/SH one, either nil when the ID has none.
type EffectiveEntry struct {
	Id        string
	Enabled   bool
	Primary   *ArtifactRef
	Candidate *ArtifactRef
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

// ChangeKind classifies why an upserted entry differs from the last commit; computed by
// internal/runtime/controller.ClassifyChanges and carried on SnapshotUpdate.
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

// Snapshot is a sorted set of EffectiveEntry consumed by executors; Generation is either the
// controller's fleet-wide DB generation or a reader's per-pod change token, never both.
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

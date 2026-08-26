package controller

import (
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"go.yaml.in/yaml/v4"
)

// CatalogGroup holds all YAML entries sharing a logical plugin ID (healthy: one BG Primary + at most one CN/SH Candidate).
type CatalogGroup[T plugin.Artifact] struct {
	Id      string
	Entries []T
}

// ValidateGroup returns rollout-policy violations for a catalog group.
func ValidateGroup[T plugin.Artifact](group CatalogGroup[T]) []string {
	var bgCount, altCount int
	for _, item := range group.Entries {
		switch item.Metadata().RolloutMode {
		case runtime.RolloutModeCanary, runtime.RolloutModeShadow:
			altCount++
		default:
			bgCount++
		}
	}
	var errs []string
	switch {
	case len(group.Entries) > 0 && bgCount == 0:
		errs = append(errs, "all-shadow group: at least one blue-green entry is required as a stable baseline")
	case bgCount > 1:
		errs = append(errs, "multiple blue-green entries for the same ID; exactly one stable baseline is allowed")
	case altCount > 1:
		errs = append(errs, "multiple canary/shadow entries for the same ID; at most one candidate is supported at a time")
	}
	return errs
}

// ElectGroup derives an effective entry from a validated catalog group.
func ElectGroup[T plugin.Artifact](id string, group CatalogGroup[T], digests map[string]string) snapshot.EffectiveEntry {
	e := snapshot.EffectiveEntry{Id: id}
	anyEnabled := false
	for _, item := range group.Entries {
		m := item.Metadata()
		if m.Enabled {
			anyEnabled = true
		}
		ref := &snapshot.ArtifactRef{Name: m.Name, RolloutMode: m.RolloutMode, Hash: digests[m.Name]}
		if b, err := yaml.Marshal(item); err == nil {
			ref.Spec = b
		}
		switch m.RolloutMode {
		case runtime.RolloutModeCanary, runtime.RolloutModeShadow:
			e.Candidate = ref
		default:
			e.Primary = ref
		}
	}
	e.Enabled = anyEnabled
	return e
}

// SnapshotChanged reports whether entries differ from the prior snapshot.
func SnapshotChanged(next []snapshot.EffectiveEntry, prior *snapshot.Snapshot) bool {
	if prior == nil || len(next) != len(prior.Entries) {
		return true
	}
	// Ref keys fold in spec+digest, so any metadata, mode, or binary change bumps the generation.
	type key struct {
		id, primary, candidate string
		enabled                bool
	}
	refKey := func(r *snapshot.ArtifactRef) string {
		if r == nil {
			return ""
		}
		return r.Name + "\x00" + r.Hash + "\x00" + string(r.Spec)
	}
	mkKey := func(e snapshot.EffectiveEntry) key {
		return key{
			id:        e.Id,
			enabled:   e.Enabled,
			primary:   refKey(e.Primary),
			candidate: refKey(e.Candidate),
		}
	}
	priorSet := make(map[key]struct{}, len(prior.Entries))
	for _, e := range prior.Entries {
		priorSet[mkKey(e)] = struct{}{}
	}
	for _, e := range next {
		if _, ok := priorSet[mkKey(e)]; !ok {
			return true
		}
	}
	return false
}

// DiffEntries computes keyed upserts and tombstones between catalog versions.
func DiffEntries(prior *snapshot.Snapshot, next []snapshot.EffectiveEntry, records map[string]*backends.ControllerRecord) (upserts []snapshot.EffectiveEntry, tombstones []string) {
	nextByID := make(map[string]snapshot.EffectiveEntry, len(next))
	for _, e := range next {
		nextByID[e.Id] = e
	}

	if prior == nil {
		upserts = append(upserts, next...)
		for id, rec := range records {
			if rec.Status == backends.StatusAbsent {
				if _, present := nextByID[id]; !present {
					tombstones = append(tombstones, id)
				}
			}
		}
		return upserts, tombstones
	}

	priorByID := make(map[string]snapshot.EffectiveEntry, len(prior.Entries))
	for _, e := range prior.Entries {
		priorByID[e.Id] = e
	}
	for _, e := range next {
		if p, ok := priorByID[e.Id]; !ok || !snapshot.EffectiveEntryEqual(p, e) {
			upserts = append(upserts, e)
		}
	}
	for _, e := range prior.Entries {
		if _, ok := nextByID[e.Id]; !ok {
			tombstones = append(tombstones, e.Id)
		}
	}
	return upserts, tombstones
}

// ClassifyChanges pairs each upsert with why it differs from the prior commit; loader is only
// needed to isolate a rollout-percentage-only edit buried inside an otherwise-identical candidate spec.
func ClassifyChanges[T plugin.Artifact](loader plugin.Loader[T], prior *snapshot.Snapshot, upserts []snapshot.EffectiveEntry) []snapshot.EntryChange {
	var priorByID map[string]snapshot.EffectiveEntry
	if prior != nil {
		priorByID = make(map[string]snapshot.EffectiveEntry, len(prior.Entries))
		for _, e := range prior.Entries {
			priorByID[e.Id] = e
		}
	}
	changes := make([]snapshot.EntryChange, len(upserts))
	for i, next := range upserts {
		if prev, ok := priorByID[next.Id]; ok {
			changes[i] = snapshot.EntryChange{Kind: classifyChange(loader, &prev, next), Entry: next}
		} else {
			changes[i] = snapshot.EntryChange{Kind: snapshot.ChangeAdded, Entry: next}
		}
	}
	return changes
}

// artifactRefIdentity reports whether two refs name the same artifact, ignoring spec content and rollout mode.
func artifactRefIdentity(left, right *snapshot.ArtifactRef) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if left == nil {
		return true
	}
	return left.Name == right.Name && left.Hash == right.Hash
}

// classifyChange isolates why one entry differs from its prior version, drilling from a general
// update down to a same-artifact candidate's rollout-mode or percentage-only change.
func classifyChange[T plugin.Artifact](loader plugin.Loader[T], prior *snapshot.EffectiveEntry, next snapshot.EffectiveEntry) snapshot.ChangeKind {
	if prior == nil {
		return snapshot.ChangeAdded
	}
	if next.Enabled != prior.Enabled || !artifactRefIdentity(prior.Primary, next.Primary) {
		return snapshot.ChangeUpdated
	}
	if !artifactRefIdentity(prior.Candidate, next.Candidate) {
		return snapshot.ChangeUpdated
	}
	if prior.Candidate == nil {
		return snapshot.ChangeUpdated // Primary spec content changed; identity checks above already passed
	}
	if prior.Candidate.RolloutMode != next.Candidate.RolloutMode {
		return snapshot.ChangeRolloutMode
	}
	if string(prior.Candidate.Spec) == string(next.Candidate.Spec) {
		return snapshot.ChangeUpdated
	}
	prevValue, prevErr := loader.ParseSpec(next.Candidate.Name, prior.Candidate.Spec)
	nextValue, nextErr := loader.ParseSpec(next.Candidate.Name, next.Candidate.Spec)
	if prevErr == nil && nextErr == nil && loader.RolloutPct(prevValue) != loader.RolloutPct(nextValue) {
		return snapshot.ChangeTrafficSplit
	}
	return snapshot.ChangeUpdated
}

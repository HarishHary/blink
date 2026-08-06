package controller

import (
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/snapshot"
	"go.yaml.in/yaml/v4"
)

// CatalogGroup holds all YAML entries sharing a logical plugin ID (healthy: one BG Primary + at most one CN/SH Candidate).
type CatalogGroup[T plugin.Syncable] struct {
	Id      string
	Entries []T
}

// ValidateGroup checks a group's rollout policy: at least one BG (stable baseline), at most one BG,
// at most one CN/SH. Returns error strings; an empty slice means valid.
func ValidateGroup[T plugin.Syncable](group CatalogGroup[T]) []string {
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

// ElectGroup derives the EffectiveEntry for one validated group: BG → Primary, CN/SH → Candidate,
// each carrying its yaml-marshaled spec. Called per changed group (incremental), so the marshal cost
// is per-change; a marshal error leaves Spec nil (a consumer that needs it skips that artifact).
func ElectGroup[T plugin.Syncable](id string, group CatalogGroup[T], digests map[string]string) snapshot.EffectiveEntry {
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

// SnapshotChanged reports whether next differs from the prior snapshot content.
// Generation is excluded - it is set by the caller after this check.
func SnapshotChanged(next []snapshot.EffectiveEntry, prior *snapshot.Snapshot) bool {
	if prior == nil || len(next) != len(prior.Entries) {
		return true
	}
	// Each ref's spec + digest fold into its key, so a metadata-only change (same binary/mode) or a
	// binary swap (same spec, new digest) still bumps the generation. Mode rides in the spec too.
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

// DiffEntries computes the per-ID upserts + tombstones to publish on the keyed topic (one compacted
// message per ID; no whole-snapshot message). On bootstrap (prior == nil) every entry is republished
// and tombstones come from the store's StatusAbsent records - so a delete that happened while the
// controller was down is re-emitted and a cold-starting reader never resurrects the ID.
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
		if p, ok := priorByID[e.Id]; !ok || !EffectiveEntryEqual(p, e) {
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

// EffectiveEntryEqual reports whether two entries match on everything a consumer routes/configures on,
// including each artifact's spec - so a metadata-only edit (same binary) is detected as a change.
func EffectiveEntryEqual(left, right snapshot.EffectiveEntry) bool {
	return left.Id == right.Id && left.Enabled == right.Enabled &&
		ArtifactRefEqual(left.Primary, right.Primary) && ArtifactRefEqual(left.Candidate, right.Candidate)
}

func ArtifactRefEqual(left, right *snapshot.ArtifactRef) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if left == nil {
		return true
	}
	return left.Name == right.Name && left.RolloutMode == right.RolloutMode && left.Hash == right.Hash && string(left.Spec) == string(right.Spec)
}

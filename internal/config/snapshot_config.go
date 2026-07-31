package config

import (
	"sync"

	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/snapshot"
)

// SnapshotConfig[T] adapts the controller's published snapshot into the data plane's Source[T].
type SnapshotConfig[T plugin.Syncable] struct {
	logger      *logger.Logger
	loader      Loader[T] // reuse the per-type loader; only ParseSpec is called here
	mu          sync.RWMutex
	generation  int64
	initialized bool
	readerReady bool                          // a non-nil snapshot has been parsed at least once
	byName      map[string]T                  // any artifact (primary or candidate) by binary stem
	primaries   []T                           // effective (primary) items, for data-plane enumeration
	rollout     map[string]pools.RolloutEntry // by logical ID, merged across artifacts
}

// NewSnapshotConfig builds a SnapshotConfig reading from src, parsing specs with loader.ParseSpec.
func NewSnapshotConfig[T plugin.Syncable](logger *logger.Logger, loader Loader[T]) *SnapshotConfig[T] {
	return &SnapshotConfig[T]{loader: loader, logger: logger}
}

// Apply parses and atomically replaces the cache with one complete published
// snapshot. Parsing happens before acquiring the write lock.
func (s *SnapshotConfig[T]) Apply(snap *snapshot.Snapshot) {
	if snap == nil {
		return
	}

	byName := make(map[string]T, len(snap.Entries))
	rollout := make(map[string]pools.RolloutEntry, len(snap.Entries))
	primaries := make([]T, 0, len(snap.Entries))

	for _, entry := range snap.Entries {
		for index, ref := range [...]*snapshot.ArtifactRef{entry.Primary, entry.Candidate} {
			if ref == nil || len(ref.Spec) == 0 {
				continue
			}
			item, err := s.loader.ParseSpec(ref.Name, ref.Spec)
			if err != nil {
				s.logger.ErrorF(
					"snapshot config: parse spec %q (id %q): %v",
					ref.Name,
					entry.Id,
					err,
				)
				continue
			}

			byName[ref.Name] = item
			metadata := item.Metadata()
			rolloutEntry := pools.RolloutEntry{
				RolloutMode: metadata.RolloutMode,
				RolloutPct:  metadata.RolloutPct,
			}
			if existing, ok := rollout[entry.Id]; ok {
				rollout[entry.Id] = mergeRollout(existing, rolloutEntry)
			} else {
				rollout[entry.Id] = rolloutEntry
			}
			if index == 0 {
				primaries = append(primaries, item)
			}
		}
	}

	s.mu.Lock()
	s.generation = snap.Generation
	s.initialized = true
	s.byName = byName
	s.primaries = primaries
	s.rollout = rollout
	s.mu.Unlock()
}

// SetReaderReady applies the lifecycle readiness projected from the snapshot
// supervisor. The last parsed config remains available while Ready is false.
func (s *SnapshotConfig[T]) SetReaderReady(ready bool) {
	s.mu.Lock()
	s.readerReady = ready
	s.mu.Unlock()
}

func (s *SnapshotConfig[T]) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.initialized && s.readerReady
}

func (s *SnapshotConfig[T]) Generation() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// Primaries returns the effective (primary) items from the latest snapshot; candidates are excluded.
func (s *SnapshotConfig[T]) Primaries() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]T(nil), s.primaries...)
}

// ByFileName returns the item parsed from the artifact whose binary stem matches name (primary or candidate); used by the pool's per-binary rollout and rpc wrappers.
func (s *SnapshotConfig[T]) ByFileName(name string) (T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.byName[name]
	return value, ok
}

// RolloutById returns the rollout rollout for a logical plugin ID, merged across its primary and candidate artifacts (highest mode + pct wins).
func (s *SnapshotConfig[T]) RolloutById(pluginId string) pools.RolloutEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rollout[pluginId]
}

// DesiredBinaryState satisfies plugin.DesiredConfig: the desired lifecycle state for one binary by stem.
func (s *SnapshotConfig[T]) DesiredBinaryState(name string) (plugin.BinaryState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.byName[name]
	if !ok {
		return plugin.BinaryState{}, false
	}
	md := item.Metadata()
	return plugin.BinaryState{
		Id:         md.Id,
		Name:       md.Name,
		Enabled:    md.Enabled,
		Mode:       md.RolloutMode,
		RolloutPct: md.RolloutPct,
		MaxProcs:   md.MaxProcs,
	}, true
}

// mergeRollout combines two rollout entries for an ID, taking the higher mode and pct.
func mergeRollout(left, right pools.RolloutEntry) pools.RolloutEntry {
	out := left
	if right.RolloutMode > left.RolloutMode {
		out.RolloutMode = right.RolloutMode
	}
	if right.RolloutPct > left.RolloutPct {
		out.RolloutPct = right.RolloutPct
	}
	return out
}

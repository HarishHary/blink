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
	logger     *logger.Logger
	src        plugin.SnapshotSource
	loader     Loader[T] // reuse the per-type loader; only ParseSpec is called here
	mu         sync.Mutex
	generation int64
	ready      bool                          // a non-nil snapshot has been parsed at least once
	byName     map[string]T                  // any artifact (primary or candidate) by binary stem
	primaries  []T                           // effective (primary) items, for data-plane enumeration
	rollout    map[string]pools.RolloutEntry // by logical ID, merged across artifacts
}

// NewSnapshotConfig builds a SnapshotConfig reading from src, parsing specs with loader.ParseSpec.
func NewSnapshotConfig[T plugin.Syncable](logger *logger.Logger, src plugin.SnapshotSource, loader Loader[T]) *SnapshotConfig[T] {
	return &SnapshotConfig[T]{src: src, loader: loader, logger: logger}
}

// Primaries returns the effective (primary) items from the latest snapshot; candidates are excluded.
func (s *SnapshotConfig[T]) Primaries() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.primaries
}

// ByFileName returns the item parsed from the artifact whose binary stem matches name (primary or candidate); used by the pool's per-binary rollout and rpc wrappers.
func (s *SnapshotConfig[T]) ByFileName(name string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	v, ok := s.byName[name]
	return v, ok
}

// RolloutById returns the rollout rollout for a logical plugin ID, merged across its primary and candidate artifacts (highest mode + pct wins).
func (s *SnapshotConfig[T]) RolloutById(id string) pools.RolloutEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.rollout[id]
}

// DesiredBinaryState satisfies plugin.DesiredConfig: the desired lifecycle state for one binary by stem.
func (s *SnapshotConfig[T]) DesiredBinaryState(name string) (plugin.BinaryState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	item, ok := s.byName[name]
	if !ok {
		return plugin.BinaryState{}, false
	}
	md := item.Metadata()
	return plugin.BinaryState{
		Id:       md.Id,
		Name:     md.Name,
		Enabled:  md.Enabled,
		Mode:     md.RolloutMode,
		MaxProcs: md.MaxProcs,
	}, true
}

// loadLocked refreshes the parsed caches on a snapshot generation change; caller holds s.mu.
func (s *SnapshotConfig[T]) loadLocked() {
	snap := s.src.Snapshot()
	if snap == nil {
		s.byName, s.primaries, s.rollout, s.ready = nil, nil, nil, false
		return
	}
	if s.ready && s.generation == snap.Generation {
		return
	}

	byName := make(map[string]T, len(snap.Entries))
	rollout := make(map[string]pools.RolloutEntry, len(snap.Entries))
	var primaries []T
	for _, e := range snap.Entries {
		// index 0 = primary (the effective item for rollout), 1 = candidate.
		for i, ref := range [...]*snapshot.ArtifactRef{e.Primary, e.Candidate} {
			if ref == nil || len(ref.Spec) == 0 {
				continue
			}
			item, err := s.loader.ParseSpec(ref.Name, ref.Spec)
			if err != nil {
				s.logger.ErrorF("snapshot config: parse spec %q (id %q): %v", ref.Name, e.Id, err)
				continue
			}
			byName[ref.Name] = item
			md := item.Metadata()
			re := pools.RolloutEntry{RolloutMode: md.RolloutMode, RolloutPct: md.RolloutPct}
			if ex, ok := rollout[e.Id]; ok {
				rollout[e.Id] = mergeRollout(ex, re)
			} else {
				rollout[e.Id] = re
			}
			if i == 0 {
				primaries = append(primaries, item)
			}
		}
	}

	s.byName, s.primaries, s.rollout = byName, primaries, rollout
	s.generation = snap.Generation
	s.ready = true
}

// mergeRollout combines two rollout entries for an ID, taking the higher mode and pct.
func mergeRollout(a, b pools.RolloutEntry) pools.RolloutEntry {
	out := a
	if b.RolloutMode > a.RolloutMode {
		out.RolloutMode = b.RolloutMode
	}
	if b.RolloutPct > a.RolloutPct {
		out.RolloutPct = b.RolloutPct
	}
	return out
}

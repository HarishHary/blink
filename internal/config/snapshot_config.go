package config

import (
	"sync"

	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/snapshot"
)

// SnapshotConfig[T] adapts the controller's published snapshot into the data plane's Source[T]: routing
// metadata (Primaries/ByFileName/RoutingByID) + the DesiredConfig the executor spawns from. No local YAML;
// everything it sees is already validated (controller is sole validator). Specs parsed once per generation.
type SnapshotConfig[T plugin.Syncable] struct {
	logger     *logger.Logger
	src        plugin.SnapshotSource
	loader     Loader[T] // reuse the per-type loader; only ParseSpec is called here
	mu         sync.Mutex
	generation int64
	ready      bool                          // a non-nil snapshot has been parsed at least once
	byName     map[string]T                  // any artifact (primary or candidate) by binary stem
	primaries  []T                           // effective (primary) items, for data-plane enumeration
	routing    map[string]pools.RoutingEntry // by logical ID, merged across artifacts
}

// NewSnapshotConfig builds a SnapshotConfig reading from src (a SnapshotReader in prod, a fake in tests),
// parsing specs with loader.ParseSpec. It starts nothing - src is fed by its own service.
func NewSnapshotConfig[T plugin.Syncable](logger *logger.Logger, src plugin.SnapshotSource, loader Loader[T]) *SnapshotConfig[T] {
	return &SnapshotConfig[T]{src: src, loader: loader, logger: logger}
}

// Primaries returns the effective (primary) items from the latest snapshot, for data-plane enumeration.
// Candidates are excluded (a canary is a new version of the same plugin, not a separate target); nil snapshot → nothing.
func (s *SnapshotConfig[T]) Primaries() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.primaries
}

// ByFileName returns the item parsed from the artifact whose binary stem matches name (primary or candidate); used by the pool's per-binary routing and rpc wrappers.
func (s *SnapshotConfig[T]) ByFileName(name string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	v, ok := s.byName[name]
	return v, ok
}

// RoutingByID returns the rollout routing for a logical plugin ID, merged across its primary and candidate artifacts (highest mode + pct wins).
func (s *SnapshotConfig[T]) RoutingByID(id string) pools.RoutingEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.routing[id]
}

// DesiredBinaryState satisfies plugin.DesiredConfig: the desired lifecycle state for one binary (by stem),
// derived from its snapshot spec; false when the snapshot names no such artifact (IsReady then defers the start).
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

// loadLocked refreshes the parsed caches if the snapshot generation changed. Caller
// holds s.mu.
func (s *SnapshotConfig[T]) loadLocked() {
	snap := s.src.Snapshot()
	if snap == nil {
		s.byName, s.primaries, s.routing, s.ready = nil, nil, nil, false
		return
	}
	if s.ready && s.generation == snap.Generation {
		return
	}

	byName := make(map[string]T, len(snap.Entries))
	routing := make(map[string]pools.RoutingEntry, len(snap.Entries))
	var primaries []T
	for _, e := range snap.Entries {
		// index 0 = primary (the effective item for routing), 1 = candidate.
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
			re := pools.RoutingEntry{Mode: md.RolloutMode, RolloutPct: md.RolloutPct}
			if ex, ok := routing[e.Id]; ok {
				routing[e.Id] = mergeRouting(ex, re)
			} else {
				routing[e.Id] = re
			}
			if i == 0 {
				primaries = append(primaries, item)
			}
		}
	}

	s.byName, s.primaries, s.routing = byName, primaries, routing
	s.generation = snap.Generation
	s.ready = true
}

// mergeRouting combines two rollout routing entries for the same logical ID, taking the higher
// mode and rollout percentage so a running canary/shadow is never under-counted.
func mergeRouting(a, b pools.RoutingEntry) pools.RoutingEntry {
	out := a
	if b.Mode > a.Mode {
		out.Mode = b.Mode
	}
	if b.RolloutPct > a.RolloutPct {
		out.RolloutPct = b.RolloutPct
	}
	return out
}

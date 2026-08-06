package plugin

import (
	"fmt"
	"sync"

	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot"
)

// Catalog is the typed, read-only projection of the latest snapshot generation.
type Catalog[T Syncable] struct {
	logger *logger.Logger
	loader Loader[T]

	mu            sync.RWMutex
	generation    int64
	initialized   bool
	valid         bool
	readerReady   bool
	serving       bool
	primaries     []T
	byFileName    map[string]T
	primaryByID   map[string]T
	candidateByID map[string]T
	rolloutByID   map[string]runtime.RolloutEntry
}

// NewCatalog creates an empty snapshot catalog.
func NewCatalog[T Syncable](logger *logger.Logger, loader Loader[T]) *Catalog[T] {
	return &Catalog[T]{logger: logger, loader: loader}
}

// Apply atomically replaces the catalog with one complete snapshot generation.
func (c *Catalog[T]) Apply(snap *snapshot.Snapshot) error {
	if snap == nil {
		return nil
	}

	byName := make(map[string]T, len(snap.Entries))
	rollout := make(map[string]runtime.RolloutEntry, len(snap.Entries))
	primaries := make([]T, 0, len(snap.Entries))
	primaryByID := make(map[string]T, len(snap.Entries))
	candidateByID := make(map[string]T, len(snap.Entries))
	var parseErr error
	for _, entry := range snap.Entries {
		for index, ref := range [...]*snapshot.ArtifactRef{entry.Primary, entry.Candidate} {
			if ref == nil || len(ref.Spec) == 0 {
				continue
			}
			item, err := c.loader.ParseSpec(ref.Name, ref.Spec)
			if err != nil {
				if parseErr == nil {
					parseErr = fmt.Errorf("parse spec %q (id %q): %w", ref.Name, entry.Id, err)
				}
				if c.logger != nil {
					c.logger.ErrorF("snapshot catalog: parse spec %q (id %q): %v", ref.Name, entry.Id, err)
				}
				continue
			}

			byName[ref.Name] = item
			metadata := item.Metadata()
			candidate := runtime.RolloutEntry{RolloutMode: metadata.RolloutMode, RolloutPct: metadata.RolloutPct}
			if current, ok := rollout[entry.Id]; ok {
				rollout[entry.Id] = mergeCatalogRollout(current, candidate)
			} else {
				rollout[entry.Id] = candidate
			}
			if index == 0 {
				primaries = append(primaries, item)
				primaryByID[entry.Id] = item
			} else {
				candidateByID[entry.Id] = item
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized && snap.Generation <= c.generation {
		return nil
	}
	if parseErr != nil {
		c.valid = false
		return parseErr
	}
	c.generation = snap.Generation
	c.initialized = true
	c.valid = true
	c.byFileName = byName
	c.primaries = primaries
	c.primaryByID = primaryByID
	c.candidateByID = candidateByID
	c.rolloutByID = rollout
	return nil
}

// PrimaryByID returns the primary metadata for one logical plugin ID.
func (c *Catalog[T]) PrimaryByID(id string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.primaryByID[id]
	return item, ok
}

// CandidateByID returns the candidate metadata for one logical plugin ID.
func (c *Catalog[T]) CandidateByID(id string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.candidateByID[id]
	return item, ok
}

// RolloutById returns the merged rollout policy for a logical plugin ID.
func (c *Catalog[T]) RolloutById(id string) runtime.RolloutEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rolloutByID[id]
}

// SetReaderReady projects snapshot-reader health without discarding the last catalog.
func (c *Catalog[T]) SetReaderReady(ready bool) {
	c.mu.Lock()
	c.readerReady = ready
	c.mu.Unlock()
}

// SetServing controls whether callers may consume catalog data. Reconciliation
// closes serving while typed metadata and plugin routing move generations.
func (c *Catalog[T]) SetServing(serving bool) {
	c.mu.Lock()
	c.serving = serving
	c.mu.Unlock()
}

// Ready reports whether a complete snapshot has been applied and its reader is healthy.
func (c *Catalog[T]) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.initialized && c.valid && c.readerReady && c.serving
}

// Generation returns the latest applied snapshot generation.
func (c *Catalog[T]) Generation() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation
}

// Primaries returns the effective stable metadata.
func (c *Catalog[T]) Primaries() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]T(nil), c.primaries...)
}

// ByFileName returns metadata for a primary or candidate artifact.
func (c *Catalog[T]) ByFileName(name string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.byFileName[name]
	return item, ok
}

// DesiredBinaryState returns the runtime lifecycle state for one artifact.
func (c *Catalog[T]) DesiredBinaryState(name string) (BinaryState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.byFileName[name]
	if !ok {
		return BinaryState{}, false
	}
	metadata := item.Metadata()
	return BinaryState{
		Id:         metadata.Id,
		Name:       metadata.Name,
		Enabled:    metadata.Enabled,
		Mode:       metadata.RolloutMode,
		RolloutPct: metadata.RolloutPct,
		MaxProcs:   metadata.MaxProcs,
	}, true
}

func mergeCatalogRollout(left, right runtime.RolloutEntry) runtime.RolloutEntry {
	if right.RolloutMode > left.RolloutMode {
		left.RolloutMode = right.RolloutMode
	}
	if right.RolloutPct > left.RolloutPct {
		left.RolloutPct = right.RolloutPct
	}
	return left
}

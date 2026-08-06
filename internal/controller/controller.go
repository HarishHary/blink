package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/snapshot"
)

// PluginController[T] is the generic control-plane service (one per plugin type): it layers the stateful
// concerns (Postgres history, generation, last-known-good carry-forward) on the LocalReader's stateless
// election and publishes the effective Snapshot. The reconcile step sequence is inline in reconcile().
type PluginController[T plugin.Syncable] struct {
	logger *logger.Logger
	db     backends.Database
	reader *LocalReader[T] // disk source: parses + elects sidecars into a Snapshot (SnapshotReader's disk twin)
	writer brokers.Writer  // bound to the snapshot topic; reconcile publishes changed snapshots here

	mu       sync.RWMutex
	snapshot *snapshot.Snapshot // prior snapshot, for reconcile change-detection
}

// NewPluginController creates a controller for type T. reader is the disk source (its own service; the
// controller reconciles on its Subscribe() signal); writer must already be bound to the snapshot topic.
func NewPluginController[T plugin.Syncable](logger *logger.Logger, db backends.Database, reader *LocalReader[T], writer brokers.Writer) *PluginController[T] {
	return &PluginController[T]{
		logger: logger,
		db:     db,
		reader: reader,
		writer: writer,
	}
}

// Start seeds the prior snapshot, subscribes to the LocalReader, runs the bootstrap reconcile, then
// re-reconciles on each reader signal until ctx is cancelled. Subscribe-before-bootstrap means a
// concurrent reader update is never missed (cap-1 signal; reconcile is idempotent).
func (c *PluginController[T]) Start(ctx context.Context) error {
	// Seed the prior snapshot so carry-forward and generation change-detection survive a restart;
	// without it bootstrap treats every entry as new (bumping generation) and drops invalid groups
	// instead of carrying them forward. Load failure is non-fatal (empty-prior fallback).
	if snap, err := c.db.LoadSnapshot(ctx); err != nil {
		c.logger.ErrorF("controller: load prior snapshot: %v (starting with empty prior)", err)
	} else if snap != nil {
		c.mu.Lock()
		c.snapshot = snap
		c.mu.Unlock()
		c.logger.Info("controller: seeded prior snapshot from store (generation=%d, entries=%d)", snap.Generation, len(snap.Entries))
	}

	ch, unsubscribe := c.reader.Subscribe()
	if err := c.reconcile(ctx, "bootstrap"); err != nil {
		unsubscribe()
		return err
	}

	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				if err := c.reconcile(ctx, "change"); err != nil {
					c.logger.ErrorF("controller: reconcile error: %v", err)
				}
			}
		}
	}()

	return nil
}

func (c *PluginController[T]) reconcile(ctx context.Context, reason string) error {
	c.logger.Info("controller: reconciling (%s)...", reason)

	// -- Step 1: load Postgres state ----------------------------------------------
	records, err := c.db.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("load Postgres records: %w", err)
	}
	generation, err := c.db.LoadGeneration(ctx)
	if err != nil {
		return fmt.Errorf("load generation: %w", err)
	}
	byID := make(map[string]*backends.ControllerRecord, len(records))
	for i := range records {
		byID[records[i].Id] = &records[i]
	}

	// -- Step 2: read the present logical IDs (incl. currently-invalid groups) -----
	// LocalReader has already re-parsed/re-elected before signalling. IDs() reports every logical ID
	// on disk (incl. groups election dropped as invalid) so absent-detection + carry-forward stay
	// exact; the elected entries come from Snapshot().
	presentIDs := make(map[string]struct{})
	for _, id := range c.reader.IDs() {
		presentIDs[id] = struct{}{}
	}

	// -- Step 3: reconcile YAML ↔ Postgres ----------------------------------------
	now := time.Now()
	upsertBatch := make([]backends.ControllerRecord, 0, len(presentIDs))

	for id := range presentIDs {
		rec, known := byID[id]
		if !known {
			// New plugin - create its Postgres record.
			newRec := backends.ControllerRecord{Id: id, FirstSeenAt: now, Status: backends.StatusActive}
			rec = &newRec
			c.logger.Info("controller: new plugin discovered: %s", id)
		}
		rec.LastSeenAt = now
		rec.Status = backends.StatusActive
		upsertBatch = append(upsertBatch, *rec)
	}
	for id, rec := range byID {
		if _, inYAML := presentIDs[id]; !inYAML && rec.Status == backends.StatusActive {
			// Was active, now absent from YAML - mark absent but keep history.
			rec.Status = backends.StatusAbsent
			upsertBatch = append(upsertBatch, *rec)
			c.logger.Info("controller: plugin absent from YAML: %s", id)
		}
	}

	// -- Steps 4-5: take the base election from the LocalReader; carry forward invalid groups --
	c.mu.RLock()
	priorSnap := c.snapshot
	c.mu.RUnlock()

	// Copy the reader's elected entries (never mutating its slice) and layer on the one stateful
	// concern it omits: last-known-good carry-forward. Snapshot() may be nil before the reader's
	// first election; we reconcile again on its next signal.
	var nextEntries []snapshot.EffectiveEntry
	if snap := c.reader.Snapshot(); snap != nil {
		nextEntries = append(nextEntries, snap.Entries...)
	}

	// Carry forward prior state for IDs still in YAML but currently invalid (dropped by election),
	// so executors keep the last-known-good config until the YAML is fixed.
	if priorSnap != nil {
		validIDs := make(map[string]struct{}, len(nextEntries))
		for _, e := range nextEntries {
			validIDs[e.Id] = struct{}{}
		}
		for _, priorEntry := range priorSnap.Entries {
			if _, ok := validIDs[priorEntry.Id]; !ok {
				if _, inYAML := presentIDs[priorEntry.Id]; inYAML {
					nextEntries = append(nextEntries, priorEntry)
				}
			}
		}
	}

	// -- Step 6: bump generation only if content changed --------------------------
	changed := SnapshotChanged(nextEntries, priorSnap)
	if changed {
		generation++
	}
	nextSnap := &snapshot.Snapshot{Generation: generation, Entries: nextEntries}

	// -- Step 7: persist reconciled records ---------------------------------------
	if err := c.db.Upsert(ctx, upsertBatch); err != nil {
		return fmt.Errorf("upsert Postgres records: %w", err)
	}

	// -- Step 8: publish and persist new snapshot ---------------------------------
	if changed {
		// Publish per-ID: one keyed message per changed entry + a nil-value tombstone per removed ID.
		// The log-compacted topic converges to one message per key, so a cold reader assembles state
		// from the compacted set and editing one rule republishes one small message, not all of them.
		upserts, tombstones := DiffEntries(priorSnap, nextEntries, byID)
		msgs := make([]brokers.Message, 0, len(upserts)+len(tombstones)+1)
		for _, e := range upserts {
			b, err := snapshot.Marshal(e)
			if err != nil {
				return fmt.Errorf("marshal snapshot entry %q: %w", e.Id, err)
			}
			msgs = append(msgs, brokers.Message{Key: []byte(e.Id), Value: b})
		}
		for _, id := range tombstones {
			msgs = append(msgs, brokers.Message{Key: []byte(id), Value: nil}) // tombstone → delete
		}
		// Generation marker last, as a barrier: on the single-partition log-compacted topic a reader
		// sees it only after all entry writes above, so a pod reporting generation N has applied all
		// of N's entries (rollout tracking via SnapshotReader.AppliedGeneration).
		msgs = append(msgs, brokers.Message{
			Key:   []byte(snapshot.GenerationMarkerKey),
			Value: snapshot.EncodeGeneration(generation),
		})
		if err := c.writer.WriteMessages(ctx, msgs...); err != nil {
			return fmt.Errorf("publish snapshot entries: %w", err)
		}

		if err := c.db.SaveSnapshot(ctx, *nextSnap); err != nil {
			return fmt.Errorf("save snapshot: %w", err)
		}
		if err := c.db.SaveGeneration(ctx, generation); err != nil {
			return fmt.Errorf("save generation: %w", err)
		}
	}

	// Publish has succeeded, so future reconciles can use this as the prior snapshot.
	c.mu.Lock()
	c.snapshot = nextSnap
	c.mu.Unlock()

	c.logger.Info("controller: reconcile done (generation=%d, entries=%d, changed=%v)",
		generation, len(nextEntries), changed)
	return nil
}

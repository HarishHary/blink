// Package backends holds the control-plane persistence layer: the Database
// interface and its implementations (SQLite, Postgres, and a no-op). One
// database can serve every plugin type - the namespace is fixed at construction.
package backends

import (
	"context"

	"github.com/harishhary/blink/internal/snapshot"
)

// Database is the persistence interface PluginController uses for bootstrap and
// reconcile. Construct an implementation with NewSQLite, NewPostgres, or NewNop.
type Database interface {
	// LoadAll returns all ControllerRecords for this database's namespace.
	LoadAll(ctx context.Context) ([]ControllerRecord, error)
	// Upsert inserts or updates the given records (insert on conflict → update).
	Upsert(ctx context.Context, records []ControllerRecord) error
	// LoadGeneration returns the last published generation number.
	LoadGeneration(ctx context.Context) (int64, error)
	// SaveGeneration persists the current generation number.
	SaveGeneration(ctx context.Context, generation int64) error
	// LoadSnapshot returns the most recently persisted snapshot for this namespace, or nil if
	// none has been saved yet. The controller seeds its prior snapshot from this on bootstrap so
	// last-known-good carry-forward and generation change-detection survive a restart.
	LoadSnapshot(ctx context.Context) (*snapshot.Snapshot, error)
	// SaveSnapshot persists the full effective snapshot for executor polling and audit.
	SaveSnapshot(ctx context.Context, snapshot snapshot.Snapshot) error
}

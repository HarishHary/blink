// Package backends holds the control-plane persistence layer: the Database
// interface and its implementations (SQLite, Postgres, and a no-op). One
// database can serve every plugin type — the namespace is fixed at construction.
package backends

import (
	"context"

	"github.com/harishhary/blink/internal/controller/model"
)

// Database is the persistence interface PluginController uses for bootstrap and
// reconcile. Construct an implementation with NewSQLite, NewPostgres, or NewNop.
type Database interface {
	// LoadAll returns all ControllerRecords for this database's namespace.
	LoadAll(ctx context.Context) ([]model.ControllerRecord, error)
	// Upsert inserts or updates the given records (insert on conflict → update).
	Upsert(ctx context.Context, records []model.ControllerRecord) error
	// LoadGeneration returns the last published generation number.
	LoadGeneration(ctx context.Context) (int64, error)
	// SaveGeneration persists the current generation number.
	SaveGeneration(ctx context.Context, gen int64) error
	// SaveSnapshot persists the full effective snapshot for executor polling and audit.
	SaveSnapshot(ctx context.Context, snap model.Snapshot) error
}

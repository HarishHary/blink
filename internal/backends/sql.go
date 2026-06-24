package backends

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/harishhary/blink/internal/controller/model"
)

// OpenSQLite opens a SQLite database at dsn (a file path, or ":memory:" for tests).
// The mattn/go-sqlite3 driver is registered by this package.
func OpenSQLite(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", dsn, err)
	}
	return db, nil
}

// NewSQLite returns a SQLite-backed Database scoped to namespace.
func NewSQLite(db *sql.DB, namespace string) (Database, error) {
	return newSQL(db, false, namespace)
}

// NewPostgres returns a Postgres-backed Database over an already-open *sql.DB.
// Register a driver (e.g. _ "github.com/lib/pq") and open the connection in main;
// this package stays driver-agnostic so importers don't all link a Postgres driver.
func NewPostgres(db *sql.DB, namespace string) (Database, error) {
	return newSQL(db, true, namespace)
}

// sqlDatabase is a database/sql-backed Database scoped to one plugin-type
// namespace. It backs both NewSQLite and NewPostgres; the only dialect
// difference is the placeholder style, handled by rebind.
type sqlDatabase struct {
	db        *sql.DB
	namespace string
	postgres  bool // true → $N placeholders; false → ? placeholders
}

// newSQL wraps db as a Database for namespace and ensures the schema exists.
func newSQL(db *sql.DB, postgres bool, namespace string) (*sqlDatabase, error) {
	s := &sqlDatabase{db: db, namespace: namespace, postgres: postgres}
	if err := s.migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("backends: migrate: %w", err)
	}
	return s, nil
}

// rebind converts ?-placeholders to $N for Postgres; identity for SQLite.
func (s *sqlDatabase) rebind(q string) string {
	if !s.postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// Timestamps are stored as Unix seconds (portable across drivers); validation
// errors and snapshots as JSON text.
const schema = `
CREATE TABLE IF NOT EXISTS controller_records (
	namespace         TEXT   NOT NULL,
	id                TEXT   NOT NULL,
	first_seen_at     BIGINT NOT NULL,
	last_seen_at      BIGINT NOT NULL,
	status            TEXT   NOT NULL,
	validation_errors TEXT   NOT NULL,
	PRIMARY KEY (namespace, id)
);
CREATE TABLE IF NOT EXISTS controller_meta (
	namespace  TEXT   NOT NULL PRIMARY KEY,
	generation BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS controller_snapshots (
	namespace  TEXT   NOT NULL,
	generation BIGINT NOT NULL,
	snapshot   TEXT   NOT NULL,
	created_at BIGINT NOT NULL,
	PRIMARY KEY (namespace, generation)
);`

func (s *sqlDatabase) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// LoadAll returns all ControllerRecords for this database's namespace.
func (s *sqlDatabase) LoadAll(ctx context.Context) ([]model.ControllerRecord, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT id, first_seen_at, last_seen_at, status, validation_errors
		   FROM controller_records WHERE namespace = ?`), s.namespace)
	if err != nil {
		return nil, fmt.Errorf("backends: load records: %w", err)
	}
	defer rows.Close()

	var out []model.ControllerRecord
	for rows.Next() {
		var (
			rec                 model.ControllerRecord
			firstSeen, lastSeen int64
			status, verrs       string
		)
		if err := rows.Scan(&rec.ID, &firstSeen, &lastSeen, &status, &verrs); err != nil {
			return nil, fmt.Errorf("backends: scan record: %w", err)
		}
		rec.FirstSeenAt = time.Unix(firstSeen, 0).UTC()
		rec.LastSeenAt = time.Unix(lastSeen, 0).UTC()
		rec.Status = model.RecordStatus(status)
		if err := json.Unmarshal([]byte(verrs), &rec.ValidationErrors); err != nil {
			return nil, fmt.Errorf("backends: decode validation_errors for %q: %w", rec.ID, err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Upsert inserts or updates records. first_seen_at is preserved on conflict.
func (s *sqlDatabase) Upsert(ctx context.Context, records []model.ControllerRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backends: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	stmt := s.rebind(
		`INSERT INTO controller_records
		   (namespace, id, first_seen_at, last_seen_at, status, validation_errors)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (namespace, id) DO UPDATE SET
		   last_seen_at      = excluded.last_seen_at,
		   status            = excluded.status,
		   validation_errors = excluded.validation_errors`)

	for _, rec := range records {
		verrs, err := json.Marshal(rec.ValidationErrors)
		if err != nil {
			return fmt.Errorf("backends: encode validation_errors for %q: %w", rec.ID, err)
		}
		if _, err := tx.ExecContext(ctx, stmt,
			s.namespace, rec.ID, rec.FirstSeenAt.Unix(), rec.LastSeenAt.Unix(),
			string(rec.Status), string(verrs),
		); err != nil {
			return fmt.Errorf("backends: upsert %q: %w", rec.ID, err)
		}
	}
	return tx.Commit()
}

// LoadGeneration returns the stored generation for this namespace, or 0 if none.
func (s *sqlDatabase) LoadGeneration(ctx context.Context) (int64, error) {
	var gen int64
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT generation FROM controller_meta WHERE namespace = ?`), s.namespace).Scan(&gen)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("backends: load generation: %w", err)
	}
	return gen, nil
}

// SaveGeneration persists the current generation for this namespace.
func (s *sqlDatabase) SaveGeneration(ctx context.Context, gen int64) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO controller_meta (namespace, generation) VALUES (?, ?)
		 ON CONFLICT (namespace) DO UPDATE SET generation = excluded.generation`),
		s.namespace, gen)
	if err != nil {
		return fmt.Errorf("backends: save generation: %w", err)
	}
	return nil
}

// SaveSnapshot persists the full snapshot as JSON, keyed by generation.
func (s *sqlDatabase) SaveSnapshot(ctx context.Context, snap model.Snapshot) error {
	blob, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("backends: encode snapshot: %w", err)
	}
	_, err = s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO controller_snapshots (namespace, generation, snapshot, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (namespace, generation) DO UPDATE SET
		   snapshot   = excluded.snapshot,
		   created_at = excluded.created_at`),
		s.namespace, snap.Generation, string(blob), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("backends: save snapshot: %w", err)
	}
	return nil
}

var _ Database = (*sqlDatabase)(nil)

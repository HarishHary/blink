package backends

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/harishhary/blink/internal/runtime/snapshot"
)

// Each controller sub-application (rule, matcher, tuning, formatter, enrichment) opens its own
// connection pool against the same underlying file, so brief cross-pool lock contention is normal,
// not exceptional - especially at boot, when every pool's first write lands at once. A small
// cancellation-blind busy sleep lets SQLite absorb that without bubbling SQLITE_BUSY up as an
// error; anything that outlasts it still returns promptly into the controller's own bounded retry
// loop, which remains the backstop for genuinely stuck contention.
const sqliteBusyTimeout = 200 * time.Millisecond

// OpenSQLite opens a SQLite database at dsn (a file path, or ":memory:" for tests).
// The mattn/go-sqlite3 driver is registered by this package.
func OpenSQLite(dsn string) (*sql.DB, error) {
	boundedDSN, err := withBoundedSQLiteBusyTimeout(dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: parse %q: %w", dsn, err)
	}
	db, err := sql.Open("sqlite3", boundedDSN)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", dsn, err)
	}
	return db, nil
}

func withBoundedSQLiteBusyTimeout(dsn string) (string, error) {
	if dsn == "" {
		return "", fmt.Errorf("empty DSN")
	}
	if dsn == ":memory:" {
		return dsn, nil
	}
	base, rawQuery, _ := strings.Cut(dsn, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", err
	}
	timeout := sqliteBusyTimeout.Milliseconds()
	for _, key := range []string{"_busy_timeout", "_timeout"} {
		if configured := query.Get(key); configured != "" {
			milliseconds, err := strconv.ParseInt(configured, 10, 64)
			if err != nil || milliseconds < 0 {
				return "", fmt.Errorf("invalid %s %q", key, configured)
			}
			if milliseconds < timeout {
				timeout = milliseconds
			}
		}
		query.Del(key)
	}
	query.Set("_busy_timeout", strconv.FormatInt(timeout, 10))
	return base + "?" + query.Encode(), nil
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
func (s *sqlDatabase) LoadAll(ctx context.Context) ([]ControllerRecord, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT id, first_seen_at, last_seen_at, status, validation_errors
		   FROM controller_records WHERE namespace = ?`), s.namespace)
	if err != nil {
		return nil, fmt.Errorf("backends: load records: %w", err)
	}
	defer rows.Close()

	var out []ControllerRecord
	for rows.Next() {
		var (
			record              ControllerRecord
			firstSeen, lastSeen int64
			status, verrs       string
		)
		if err := rows.Scan(&record.Id, &firstSeen, &lastSeen, &status, &verrs); err != nil {
			return nil, fmt.Errorf("backends: scan record: %w", err)
		}
		record.FirstSeenAt = time.Unix(firstSeen, 0).UTC()
		record.LastSeenAt = time.Unix(lastSeen, 0).UTC()
		record.Status = RecordStatus(status)
		if err := json.Unmarshal([]byte(verrs), &record.ValidationErrors); err != nil {
			return nil, fmt.Errorf("backends: decode validation_errors for %q: %w", record.Id, err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// Upsert inserts or updates records. first_seen_at is preserved on conflict.
func (s *sqlDatabase) Upsert(ctx context.Context, records []ControllerRecord) error {
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

	for _, record := range records {
		verrs, err := json.Marshal(record.ValidationErrors)
		if err != nil {
			return fmt.Errorf("backends: encode validation_errors for %q: %w", record.Id, err)
		}
		if _, err := tx.ExecContext(ctx, stmt,
			s.namespace, record.Id, record.FirstSeenAt.Unix(), record.LastSeenAt.Unix(),
			string(record.Status), string(verrs),
		); err != nil {
			return fmt.Errorf("backends: upsert %q: %w", record.Id, err)
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
func (s *sqlDatabase) SaveGeneration(ctx context.Context, generation int64) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO controller_meta (namespace, generation) VALUES (?, ?)
		 ON CONFLICT (namespace) DO UPDATE SET generation = excluded.generation`),
		s.namespace, generation)
	if err != nil {
		return fmt.Errorf("backends: save generation: %w", err)
	}
	return nil
}

// SaveSnapshot persists the full snapshot as JSON, keyed by generation.
func (s *sqlDatabase) SaveSnapshot(ctx context.Context, snapshot snapshot.Snapshot) error {
	blob, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("backends: encode snapshot: %w", err)
	}
	_, err = s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO controller_snapshots (namespace, generation, snapshot, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (namespace, generation) DO UPDATE SET
		   snapshot   = excluded.snapshot,
		   created_at = excluded.created_at`),
		s.namespace, snapshot.Generation, string(blob), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("backends: save snapshot: %w", err)
	}
	return nil
}

// LoadSnapshot returns the most recently persisted snapshot for this namespace (highest
// generation), or nil if none has been saved. Read side of SaveSnapshot: the controller seeds
// its prior snapshot from this on bootstrap so carry-forward and change-detection survive a restart.
func (s *sqlDatabase) LoadSnapshot(ctx context.Context) (*snapshot.Snapshot, error) {
	var blob string
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT snapshot FROM controller_snapshots WHERE namespace = ?
		 ORDER BY generation DESC LIMIT 1`), s.namespace).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backends: load snapshot: %w", err)
	}
	var snap snapshot.Snapshot
	if err := json.Unmarshal([]byte(blob), &snap); err != nil {
		return nil, fmt.Errorf("backends: decode snapshot: %w", err)
	}
	return &snap, nil
}

var _ Database = (*sqlDatabase)(nil)

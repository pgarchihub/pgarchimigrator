package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required — matches every other SQLite-backed package in this project
)

// timeLayout mirrors every other SQLite-backed package's own layout —
// a fixed-width, always-UTC format so string comparison (used by
// DeleteExpired's own `WHERE created_at < ?`) matches real
// chronological order.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// DefaultRetention is how long a Record is kept before DeleteExpired
// removes it — AC-PF-003 Section 12.2's own "a retry for the same
// logical request" scenario is about a caller retrying within a bounded window
// (a timeout, a network blip), not an indefinite guarantee; 24 hours is
// a generous window for that without the table growing unboundedly for
// an instance that's been running for months.
const DefaultRetention = 24 * time.Hour

const createSchemaSQL = `
CREATE TABLE IF NOT EXISTS idempotency_records (
	key           TEXT PRIMARY KEY,
	status_code   INTEGER NOT NULL,
	response_body BLOB NOT NULL,
	created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_idempotency_records_created_at ON idempotency_records (created_at);
`

// SQLiteStore is the SQLite-backed Store implementation — its own
// separate database file (see NewSQLiteStore), same "isolate this
// write path" reasoning as every other SQLite-backed store in this
// project, though a caller wiring this product together is free to
// point it at an existing file (e.g. the shared auth database) instead
// — SQLite has no objection to multiple unrelated table sets in one
// file, the same wiring-level convenience internal/serviceauth's own
// SQLiteStore doc comment already notes.
type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite (%s): %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer, same rationale as every other store in this project

	if _, err := db.Exec(createSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create idempotency schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) Get(ctx context.Context, key string) (*Record, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT key, status_code, response_body, created_at FROM idempotency_records WHERE key = ?`, key)

	var r Record
	var createdAt string
	if err := row.Scan(&r.Key, &r.StatusCode, &r.ResponseBody, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to scan idempotency record: %w", err)
	}
	parsed, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	r.CreatedAt = parsed
	return &r, nil
}

func (s *SQLiteStore) Put(ctx context.Context, record *Record) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency_records (key, status_code, response_body, created_at) VALUES (?, ?, ?, ?)`,
		record.Key, record.StatusCode, record.ResponseBody, record.CreatedAt.Format(timeLayout),
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("failed to store idempotency record: %w", err)
	}
	return nil
}

// DeleteExpired removes every record older than DefaultRetention and
// returns how many were deleted — intended to be called from a
// periodic background loop (matching engines/postgresql/reaper's own role for
// migration jobs), so this table doesn't grow forever on a
// long-running instance.
func (s *SQLiteStore) DeleteExpired(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-DefaultRetention).Format(timeLayout)
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_records WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to delete expired idempotency records: %w", err)
	}
	return res.RowsAffected()
}

// isUniqueConstraintErr checks for SQLite's UNIQUE constraint violation
// by substring match — same technique and reasoning as every other
// SQLite-backed package in this project (modernc.org/sqlite doesn't
// export a typed sentinel for this).
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

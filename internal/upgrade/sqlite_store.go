package upgrade

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required — matches internal/auth/internal/serviceauth's own choice
)

// ErrNotFound is returned by every Store lookup method when the
// requested row doesn't exist — mirrors serviceauth.ErrNotFound's exact
// role, kept as this package's own distinct value for the same
// "zero cross-package import dependency" reasoning that package's own
// ErrNotFound documents.
var ErrNotFound = errors.New("not found")

// timeLayout mirrors internal/auth/internal/serviceauth's own
// SQLiteStore identically — a fixed-width, always-UTC layout so string
// comparison stays correct chronological order.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

const createUpgradeSchemaSQL = `
CREATE TABLE IF NOT EXISTS upgrade_jobs (
	id                    TEXT PRIMARY KEY,
	phase                 TEXT NOT NULL,
	schemas               TEXT NOT NULL,
	source_connection_ref TEXT NOT NULL,
	target_connection_ref TEXT NOT NULL,
	source_replication_ref TEXT NOT NULL DEFAULT '',
	tables_json           TEXT NOT NULL DEFAULT '',
	last_error            TEXT NOT NULL DEFAULT '',
	created_at            TEXT NOT NULL,
	updated_at            TEXT NOT NULL,
	tables_total          INTEGER NOT NULL DEFAULT 0,
	tables_synced         INTEGER NOT NULL DEFAULT 0,
	tables_verified       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS upgrade_tables (
	job_id      TEXT NOT NULL,
	schema_name TEXT NOT NULL,
	table_name  TEXT NOT NULL,
	phase       TEXT NOT NULL,
	rows_synced INTEGER NOT NULL DEFAULT 0,
	last_error  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (job_id, schema_name, table_name)
);
CREATE INDEX IF NOT EXISTS idx_upgrade_tables_job_id ON upgrade_tables (job_id);
`

// SQLiteStore is the SQLite-backed implementation of Store.
//
// Deliberately its OWN database file (see NewSQLiteStore), separate
// from BOTH internal/state's migration-job checkpoint database AND
// internal/auth/internal/serviceauth's shared auth database — not
// merely following those packages' own "own file" precedent by default,
// but a specific, discussed decision: an upgrade job can be actively
// syncing many tables' progress concurrently (see UpdateTableRowsSynced's
// own doc comment for how this is kept write-frugal regardless), and
// isolating that write path into its own file means any write
// contention this generates can never add latency to ordinary schema
// migrations sharing internal/state's own database — SQLite's
// single-writer lock is per FILE, not global across every database this
// process happens to have open.
//
// This is a deliberately deferred decision, not a final one — see
// docs/ecosystem/ARCHITECTURE.md's own "Store backend" note: SQLite is
// enough for what this package needs today (a single self-hosted
// instance, no cross-instance shared state); if real-world write
// concurrency or a multi-instance HA deployment ever demands otherwise,
// Store's own interface boundary is exactly the seam a second,
// non-SQLite implementation would go behind, with no change needed to
// any of this package's own sync/validate logic.
type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore opens (creating if necessary) a SQLite database at path
// and applies the upgrade schema.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite (%s): %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer, same rationale as every other store in this project

	if _, err := db.Exec(createUpgradeSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create upgrade schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate upgrade schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// migrateAddSourceReplicationRef adds the source_replication_ref column
// to an upgrade_jobs table that was created by an OLDER version of this
// package — CREATE TABLE IF NOT EXISTS only creates the table when it
// doesn't already exist; it does NOT retroactively add a column to a
// table that already exists from a prior run. Without this migration
// step, any deployment that already had an upgrade_jobs table (even
// from a single earlier upgrade attempt) would hit "no such column:
// <new column>" the first time CreateJob tried to INSERT after
// upgrading to a version of this code that added a field — a real bug
// found exactly this way during manual testing (see
// docs/ecosystem/ARCHITECTURE.md's own note on this). Every column
// added to upgrade_jobs AFTER its original release (source_replication_ref,
// tables_json, and whatever comes next) MUST be added to
// addedColumns below, not just to createUpgradeSchemaSQL — otherwise an
// existing deployment's database file never receives it.
//
// Checks PRAGMA table_info first rather than blindly running ALTER
// TABLE and pattern-matching the error text for "duplicate column
// name" — that string match would be a real, fragile dependency on
// modernc.org/sqlite's own exact error message wording, whereas
// table_info is SQLite's own structured, documented way to ask "does
// this column already exist," true regardless of error-message
// phrasing.
func migrateSchema(db *sql.DB) error {
	existing, err := existingColumns(db, "upgrade_jobs")
	if err != nil {
		return err
	}

	// addedColumns: every column added to upgrade_jobs since its
	// original release, in the exact ALTER TABLE fragment needed to add
	// it. Append here, never remove — removing an entry would silently
	// stop migrating any database file that still predates it.
	addedColumns := []struct {
		name       string
		definition string
	}{
		{"source_replication_ref", "TEXT NOT NULL DEFAULT ''"},
		{"tables_json", "TEXT NOT NULL DEFAULT ''"},
	}

	for _, col := range addedColumns {
		if existing[col.name] {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE upgrade_jobs ADD COLUMN %s %s", col.name, col.definition)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("failed to add %s column: %w", col.name, err)
		}
	}
	return nil
}

// existingColumns returns the set of column names table currently has,
// via PRAGMA table_info — see migrateSchema's own doc comment for why
// this structured approach is used instead of matching ALTER TABLE's
// own error text.
func existingColumns(db *sql.DB, table string) (map[string]bool, error) {
	// table is always this package's own hardcoded literal
	// ("upgrade_jobs") at every call site, never external input — safe
	// to interpolate directly; PRAGMA doesn't support parameter binding
	// for its own target object the way a normal query does.
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, fmt.Errorf("failed to inspect %s schema: %w", table, err)
	}
	defer rows.Close()

	// PRAGMA table_info's own column shape: cid, name, type, notnull,
	// dflt_value, pk — only `name` is needed here, the rest are scanned
	// into discarded variables to satisfy Scan's fixed arity.
	existing := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("failed to scan %s column info: %w", table, err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return existing, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) CreateJob(ctx context.Context, job *Job) error {
	if job.ID == "" {
		job.ID = newID("upgrade")
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.Phase == "" {
		job.Phase = PhaseIntrospecting
	}

	tablesJSON, err := json.Marshal(job.Tables)
	if err != nil {
		return fmt.Errorf("failed to encode job.Tables: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO upgrade_jobs (id, phase, schemas, source_connection_ref, target_connection_ref, source_replication_ref, tables_json, last_error, created_at, updated_at, tables_total, tables_synced, tables_verified)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, string(job.Phase), strings.Join(job.Schemas, ","), job.SourceConnectionRef, job.TargetConnectionRef,
		job.SourceReplicationRef, string(tablesJSON), job.LastError, job.CreatedAt.Format(timeLayout), job.UpdatedAt.Format(timeLayout),
		job.TablesTotal, job.TablesSynced, job.TablesVerified,
	)
	if err != nil {
		return fmt.Errorf("failed to create upgrade job: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetJob(ctx context.Context, jobID string) (*Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, phase, schemas, source_connection_ref, target_connection_ref, source_replication_ref, tables_json, last_error, created_at, updated_at, tables_total, tables_synced, tables_verified
		 FROM upgrade_jobs WHERE id = ?`, jobID)
	return scanJob(row)
}

func (s *SQLiteStore) ListJobs(ctx context.Context) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, phase, schemas, source_connection_ref, target_connection_ref, source_replication_ref, tables_json, last_error, created_at, updated_at, tables_total, tables_synced, tables_verified
		 FROM upgrade_jobs ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("failed to list upgrade jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *SQLiteStore) UpdateJobPhase(ctx context.Context, jobID string, phase Phase) error {
	return s.updateJobPhase(ctx, jobID, phase, "")
}

func (s *SQLiteStore) UpdateJobPhaseWithError(ctx context.Context, jobID string, phase Phase, lastError string) error {
	return s.updateJobPhase(ctx, jobID, phase, lastError)
}

func (s *SQLiteStore) updateJobPhase(ctx context.Context, jobID string, phase Phase, lastError string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE upgrade_jobs SET phase = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		string(phase), lastError, time.Now().UTC().Format(timeLayout), jobID,
	)
	if err != nil {
		return fmt.Errorf("failed to update upgrade job phase: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateJobProgress overwrites the job's own coarse counters
// (TablesTotal/TablesSynced/TablesVerified) — see Job.TablesTotal's own
// doc comment for why these are a deliberately coarse, whole-job signal
// distinct from per-table detail. Intended to be called occasionally
// (e.g. once per table completing, not once per row) — same
// write-frugal principle as UpdateTableRowsSynced below, just at job
// granularity rather than table granularity.
func (s *SQLiteStore) UpdateJobProgress(ctx context.Context, jobID string, tablesTotal, tablesSynced, tablesVerified int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE upgrade_jobs SET tables_total = ?, tables_synced = ?, tables_verified = ?, updated_at = ? WHERE id = ?`,
		tablesTotal, tablesSynced, tablesVerified, time.Now().UTC().Format(timeLayout), jobID,
	)
	if err != nil {
		return fmt.Errorf("failed to update upgrade job progress: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) CreateTable(ctx context.Context, table *Table) error {
	if table.Phase == "" {
		table.Phase = PhaseIntrospecting
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upgrade_tables (job_id, schema_name, table_name, phase, rows_synced, last_error) VALUES (?, ?, ?, ?, ?, ?)`,
		table.JobID, table.SchemaName, table.TableName, string(table.Phase), table.RowsSynced, table.LastError,
	)
	if err != nil {
		return fmt.Errorf("failed to create upgrade table record: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListTables(ctx context.Context, jobID string) ([]*Table, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id, schema_name, table_name, phase, rows_synced, last_error FROM upgrade_tables WHERE job_id = ? ORDER BY schema_name, table_name`, jobID)
	if err != nil {
		return nil, fmt.Errorf("failed to list upgrade tables: %w", err)
	}
	defer rows.Close()

	var tables []*Table
	for rows.Next() {
		table, err := scanTable(rows)
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, rows.Err()
}

func (s *SQLiteStore) UpdateTablePhase(ctx context.Context, jobID, schemaName, tableName string, phase Phase, lastError string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE upgrade_tables SET phase = ?, last_error = ? WHERE job_id = ? AND schema_name = ? AND table_name = ?`,
		string(phase), lastError, jobID, schemaName, tableName,
	)
	if err != nil {
		return fmt.Errorf("failed to update upgrade table phase: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateTableRowsSynced overwrites one table's own row-count progress.
//
// Write-frugal by design, not by accident: the actual sync engine
// (not yet built — see docs/ecosystem/ARCHITECTURE.md's own upgrade
// section for what's built so far) is expected to call this at most a
// few times per second per table — e.g. once per batch of rows copied,
// or on a fixed timer — never once per row. A whole-database upgrade
// A whole-database upgrade
// can genuinely be copying millions of rows across many tables
// concurrently; a per-row UPDATE against a single-writer SQLite
// database would serialize every one of those writes behind SQLite's
// own write lock, turning a data-copy problem into a
// write-contention problem entirely of this package's own making. This
// method itself does nothing to enforce a minimum interval between
// calls — that discipline belongs in the caller (the sync engine), not
// here; this doc comment IS the contract callers are expected to honor.
func (s *SQLiteStore) UpdateTableRowsSynced(ctx context.Context, jobID, schemaName, tableName string, rowsSynced int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE upgrade_tables SET rows_synced = ? WHERE job_id = ? AND schema_name = ? AND table_name = ?`,
		rowsSynced, jobID, schemaName, tableName,
	)
	if err != nil {
		return fmt.Errorf("failed to update upgrade table rows_synced: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// rowScanner lets scanJob/scanTable work with both *sql.Row
// (QueryRowContext) and *sql.Rows (QueryContext) — same pattern
// internal/state.scanJob and internal/serviceauth's own scanClient/
// scanAccessToken use, for the identical reason.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	var phase, schemasRaw, tablesJSON, createdAt, updatedAt string
	if err := row.Scan(&j.ID, &phase, &schemasRaw, &j.SourceConnectionRef, &j.TargetConnectionRef, &j.SourceReplicationRef, &tablesJSON, &j.LastError,
		&createdAt, &updatedAt, &j.TablesTotal, &j.TablesSynced, &j.TablesVerified); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to scan upgrade job: %w", err)
	}
	j.Phase = Phase(phase)
	if schemasRaw != "" {
		j.Schemas = strings.Split(schemasRaw, ",")
	}
	if tablesJSON != "" {
		if err := json.Unmarshal([]byte(tablesJSON), &j.Tables); err != nil {
			return nil, fmt.Errorf("failed to decode tables_json: %w", err)
		}
	}
	parsedCreated, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	j.CreatedAt = parsedCreated
	parsedUpdated, err := time.Parse(timeLayout, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse updated_at: %w", err)
	}
	j.UpdatedAt = parsedUpdated
	return &j, nil
}

func scanTable(row rowScanner) (*Table, error) {
	var t Table
	var phase string
	if err := row.Scan(&t.JobID, &t.SchemaName, &t.TableName, &phase, &t.RowsSynced, &t.LastError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to scan upgrade table: %w", err)
	}
	t.Phase = Phase(phase)
	return &t, nil
}

func newID(prefix string) string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf) // crypto/rand.Read never partially fails in practice; error is non-actionable here
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(buf))
}

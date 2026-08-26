package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required (aligns with TR-13, single-instance deployment)
)

// ErrJobNotFound is returned by Get() when the requested job does not exist.
var ErrJobNotFound = errors.New("job not found")

// timeLayout is used DELIBERATELY instead of RFC3339Nano, at a fixed width
// (always 9 fractional digits). RFC3339Nano trims trailing zeros, producing
// variable-length strings; that can make the `updated_at < ?` string
// comparison in ListStale inconsistent with chronological order (e.g. ".5Z"
// sorts lexicographically before "Z"). Fixed width + always UTC ("Z")
// guarantees that string comparison matches real chronological order.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

const createTableSQL = `
CREATE TABLE IF NOT EXISTS jobs (
	id                    TEXT PRIMARY KEY,
	schema_name           TEXT NOT NULL,
	table_name            TEXT NOT NULL,
	strategy              TEXT NOT NULL,
	phase                 TEXT NOT NULL,
	replication_slot_name TEXT NOT NULL DEFAULT '',
	shadow_table_name     TEXT NOT NULL DEFAULT '',
	created_at            TEXT NOT NULL,
	updated_at            TEXT NOT NULL,
	rollback_deadline     TEXT,
	last_error            TEXT NOT NULL DEFAULT '',
	operation             TEXT NOT NULL DEFAULT '',
	column_name           TEXT NOT NULL DEFAULT '',
	column_type           TEXT NOT NULL DEFAULT '',
	default_value         TEXT NOT NULL DEFAULT '',
	is_volatile_default   INTEGER NOT NULL DEFAULT 0,
	deprecated_column_name TEXT NOT NULL DEFAULT '',
	index_name             TEXT NOT NULL DEFAULT '',
	index_definition       TEXT NOT NULL DEFAULT '',
	constraint_name         TEXT NOT NULL DEFAULT '',
	check_expression        TEXT NOT NULL DEFAULT '',
	new_column_name         TEXT NOT NULL DEFAULT '',
	estimated_row_count      INTEGER NOT NULL DEFAULT 0,
	rows_processed           INTEGER NOT NULL DEFAULT 0,
	name                     TEXT NOT NULL DEFAULT '',
	description              TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_jobs_phase_updated_at ON jobs (phase, updated_at);
`

// pendingColumnMigrations lists every column added to "jobs" after the
// original createTableSQL was written. CREATE TABLE IF NOT EXISTS is a
// no-op against a table that already exists under an OLDER schema, so
// these ALTER TABLE statements are what actually bring a pre-existing
// local database (e.g. a pgarchimigrator-state.db from an earlier session) up to
// date. SQLite errors on ALTER TABLE ADD COLUMN if the column already
// exists — that specific, expected error is swallowed so this is safe to
// run unconditionally on every startup, in order, forever.
var pendingColumnMigrations = []string{
	`ALTER TABLE jobs ADD COLUMN deprecated_column_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN index_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN index_definition TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN constraint_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN check_expression TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN new_column_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN estimated_row_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN rows_processed INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
	// NULL-able (no DEFAULT/NOT NULL) — nil/NULL deliberately means
	// "impact measurement was never turned on for this migration", not
	// "measured and found to be zero" (see Job.ImpactPeakQueryDurationSeconds's
	// own doc comment).
	`ALTER TABLE jobs ADD COLUMN impact_peak_query_duration_seconds REAL`,
}

func migrateSchema(db *sql.DB) error {
	for _, stmt := range pendingColumnMigrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migration %q failed: %w", stmt, err)
		}
	}
	return nil
}

// SQLiteStore is the single-instance (TR-13) SQLite implementation of Store.
// Architecture Doc Section 5.1: for HA/multi-pod scenarios this
// implementation should be swapped for a PostgreSQL-backed Store, planned as
// a separate roadmap phase.
type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore opens (creating if necessary) a SQLite database at the
// given path and applies the schema migration (CREATE TABLE IF NOT EXISTS).
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite (%s): %w", path, err)
	}

	// SQLite only supports a single writer; checkpoint writes are frequent
	// but small, so a single connection is enough and reduces lock contention.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate schema: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// Create creates a new job record for the "State Manager" described in
// Architecture Doc Section 3.1.
func (s *SQLiteStore) Create(ctx context.Context, job *Job) error {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now

	var rollbackDeadline any
	if job.RollbackDeadline != nil {
		rollbackDeadline = job.RollbackDeadline.UTC().Format(timeLayout)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (
			id, schema_name, table_name, strategy, phase,
			replication_slot_name, shadow_table_name,
			created_at, updated_at, rollback_deadline, last_error,
			operation, column_name, column_type, default_value, is_volatile_default,
			deprecated_column_name, index_name, index_definition,
			constraint_name, check_expression, new_column_name,
			estimated_row_count, rows_processed, name, description, impact_peak_query_duration_seconds
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		job.ID, job.SchemaName, job.TableName, job.Strategy, string(job.Phase),
		job.ReplicationSlotName, job.ShadowTableName,
		job.CreatedAt.Format(timeLayout), job.UpdatedAt.Format(timeLayout),
		rollbackDeadline, job.LastError,
		job.Operation, job.ColumnName, job.ColumnType, job.DefaultValue, job.IsVolatileDefault,
		job.DeprecatedColumnName, job.IndexName, job.IndexDefinition,
		job.ConstraintName, job.CheckExpression, job.NewColumnName,
		job.EstimatedRowCount, job.RowsProcessed, job.Name, job.Description,
		job.ImpactPeakQueryDurationSeconds, // nil on creation — a brand new job hasn't had impact measured yet
	)
	if err != nil {
		return fmt.Errorf("failed to create job (id=%s): %w", job.ID, err)
	}
	return nil
}

// UpdatePhase updates a job's phase and its updated_at timestamp, per FR-04
// ("progress report at every step"). It must be called at every step so the
// reaper's ListStale query can detect orphaned jobs using this field.
func (s *SQLiteStore) UpdatePhase(ctx context.Context, jobID string, phase Phase) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET phase = ?, updated_at = ? WHERE id = ?
	`, string(phase), time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to update phase (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UpdatePhaseWithError updates the phase and persists LastError at the same
// time — typically used together with phase=FAILED.
func (s *SQLiteStore) UpdatePhaseWithError(ctx context.Context, jobID string, phase Phase, lastError string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET phase = ?, updated_at = ?, last_error = ? WHERE id = ?
	`, string(phase), time.Now().UTC().Format(timeLayout), lastError, jobID)
	if err != nil {
		return fmt.Errorf("failed to update phase/error (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UpdateResources persists the replication slot and shadow table names for
// a job — see the Store interface doc comment.
func (s *SQLiteStore) UpdateResources(ctx context.Context, jobID string, slotName, shadowTableName string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET replication_slot_name = ?, shadow_table_name = ?, updated_at = ? WHERE id = ?
	`, slotName, shadowTableName, time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to update resources (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UpdateRollbackDeadline persists the FR-08a rollback window deadline.
func (s *SQLiteStore) UpdateRollbackDeadline(ctx context.Context, jobID string, deadline time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET rollback_deadline = ?, updated_at = ? WHERE id = ?
	`, deadline.UTC().Format(timeLayout), time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to update rollback deadline (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UpdateDeprecatedColumnName persists the temporary name a column was
// renamed to during DDLFlow's DROP_COLUMN "soft drop" step.
func (s *SQLiteStore) UpdateDeprecatedColumnName(ctx context.Context, jobID string, deprecatedName string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET deprecated_column_name = ?, updated_at = ? WHERE id = ?
	`, deprecatedName, time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to update deprecated column name (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UpdateIndexName persists the index name for ADD_INDEX/DROP_INDEX jobs.
func (s *SQLiteStore) UpdateIndexName(ctx context.Context, jobID string, indexName string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET index_name = ?, updated_at = ? WHERE id = ?
	`, indexName, time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to update index name (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UpdateIndexDefinition persists the pg_get_indexdef() output captured
// before a DROP_INDEX job's index is dropped.
func (s *SQLiteStore) UpdateIndexDefinition(ctx context.Context, jobID string, definition string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET index_definition = ?, updated_at = ? WHERE id = ?
	`, definition, time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to update index definition (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// UpdateConstraintName persists the constraint name for a SET_NOT_NULL job
// once DDLFlow has resolved it (auto-generating one if the caller left it empty).
func (s *SQLiteStore) UpdateConstraintName(ctx context.Context, jobID string, constraintName string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET constraint_name = ?, updated_at = ? WHERE id = ?
	`, constraintName, time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to update constraint name (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// IncrementRowsProcessed adds delta to the job's running RowsProcessed
// counter — an atomic "rows_processed = rows_processed + ?" rather than a
// read-modify-write, since the caller only ever knows how many rows THIS
// batch touched, not the running total.
func (s *SQLiteStore) IncrementRowsProcessed(ctx context.Context, jobID string, delta int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET rows_processed = rows_processed + ?, updated_at = ? WHERE id = ?
	`, delta, time.Now().UTC().Format(timeLayout), jobID)
	if err != nil {
		return fmt.Errorf("failed to increment rows processed (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

// Get returns a single job record. Returns ErrJobNotFound if it doesn't exist.
func (s *SQLiteStore) Get(ctx context.Context, jobID string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, schema_name, table_name, strategy, phase,
		       replication_slot_name, shadow_table_name,
		       created_at, updated_at, rollback_deadline, last_error,
		       operation, column_name, column_type, default_value, is_volatile_default, deprecated_column_name, index_name, index_definition, constraint_name, check_expression, new_column_name, estimated_row_count, rows_processed, name, description, impact_peak_query_duration_seconds
		FROM jobs WHERE id = ?
	`, jobID)

	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read job (id=%s): %w", jobID, err)
	}
	return job, nil
}

// ListStale returns jobs for Architecture Doc Section 3.3 "Orphan Resource
// Reaper": jobs whose last update is older than olderThan and that have not
// yet reached a terminal state (COMPLETED, FAILED, ABORTED) — these are
// likely orphaned by a crash or network interruption.
//
// ROLLBACK_WINDOW is ALSO excluded here, deliberately: a job sitting in
// ROLLBACK_WINDOW is not orphaned, it is a successfully swapped migration
// waiting out its FR-08a grace period. Treating it as "stale" here would
// race with ListExpiredRollbackWindows/SweepExpiredRollbackWindows and
// could mark a successful migration ABORTED instead of COMPLETED, and
// leak its temp table besides (ScanOnce doesn't know about temp tables at
// all — only ShadowTableName/ReplicationSlotName are persisted; see the
// design note in internal/shadowflow's resourceNamesFor).
func (s *SQLiteStore) ListStale(ctx context.Context, olderThan time.Duration) ([]*Job, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(timeLayout)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, schema_name, table_name, strategy, phase,
		       replication_slot_name, shadow_table_name,
		       created_at, updated_at, rollback_deadline, last_error,
		       operation, column_name, column_type, default_value, is_volatile_default, deprecated_column_name, index_name, index_definition, constraint_name, check_expression, new_column_name, estimated_row_count, rows_processed, name, description, impact_peak_query_duration_seconds
		FROM jobs
		WHERE updated_at < ?
		  AND phase NOT IN (?, ?, ?, ?)
	`, cutoff, string(PhaseCompleted), string(PhaseFailed), string(PhaseAborted), string(PhaseRollbackWindow))
	if err != nil {
		return nil, fmt.Errorf("failed to list stale jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListExpiredRollbackWindows returns jobs in ROLLBACK_WINDOW whose
// RollbackDeadline has passed — see the Store interface doc comment.
func (s *SQLiteStore) ListExpiredRollbackWindows(ctx context.Context) ([]*Job, error) {
	now := time.Now().UTC().Format(timeLayout)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, schema_name, table_name, strategy, phase,
		       replication_slot_name, shadow_table_name,
		       created_at, updated_at, rollback_deadline, last_error,
		       operation, column_name, column_type, default_value, is_volatile_default, deprecated_column_name, index_name, index_definition, constraint_name, check_expression, new_column_name, estimated_row_count, rows_processed, name, description, impact_peak_query_duration_seconds
		FROM jobs
		WHERE phase = ?
		  AND rollback_deadline IS NOT NULL
		  AND rollback_deadline < ?
	`, string(PhaseRollbackWindow), now)
	if err != nil {
		return nil, fmt.Errorf("failed to list expired rollback windows: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListAll returns every job, most recently created first.
func (s *SQLiteStore) ListAll(ctx context.Context) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, schema_name, table_name, strategy, phase,
		       replication_slot_name, shadow_table_name,
		       created_at, updated_at, rollback_deadline, last_error,
		       operation, column_name, column_type, default_value, is_volatile_default, deprecated_column_name, index_name, index_definition, constraint_name, check_expression, new_column_name, estimated_row_count, rows_processed, name, description, impact_peak_query_duration_seconds
		FROM jobs
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// rowScanner is the common Scan interface shared by sql.Row and sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (*Job, error) {
	var (
		job              Job
		phase            string
		createdAt        string
		updatedAt        string
		rollbackDeadline sql.NullString
		impactPeak       sql.NullFloat64
	)

	if err := row.Scan(
		&job.ID, &job.SchemaName, &job.TableName, &job.Strategy, &phase,
		&job.ReplicationSlotName, &job.ShadowTableName,
		&createdAt, &updatedAt, &rollbackDeadline, &job.LastError,
		&job.Operation, &job.ColumnName, &job.ColumnType, &job.DefaultValue, &job.IsVolatileDefault,
		&job.DeprecatedColumnName, &job.IndexName, &job.IndexDefinition,
		&job.ConstraintName, &job.CheckExpression, &job.NewColumnName,
		&job.EstimatedRowCount, &job.RowsProcessed, &job.Name, &job.Description,
		&impactPeak,
	); err != nil {
		return nil, err
	}

	job.Phase = Phase(phase)

	parsedCreated, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	job.CreatedAt = parsedCreated

	parsedUpdated, err := time.Parse(timeLayout, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse updated_at: %w", err)
	}
	job.UpdatedAt = parsedUpdated

	if rollbackDeadline.Valid {
		t, err := time.Parse(timeLayout, rollbackDeadline.String)
		if err != nil {
			return nil, fmt.Errorf("failed to parse rollback_deadline: %w", err)
		}
		job.RollbackDeadline = &t
	}

	if impactPeak.Valid {
		job.ImpactPeakQueryDurationSeconds = &impactPeak.Float64
	}

	return &job, nil
}

// UpdateImpactPeak persists the current running peak from internal/api's
// impactTracker — see Job.ImpactPeakQueryDurationSeconds's own doc
// comment. Deliberately does NOT touch updated_at, unlike most Update*
// methods here — this gets called repeatedly (write-through, on every
// poll while impact measurement is on), and updated_at's meaning
// throughout this project (the Health Card, Fleet analytics, the
// Migration Detail page's own displayed duration) is "when did this
// job's PHASE/STATE last meaningfully change", not "when was any field
// on this row last touched". Bumping it here would silently inflate a
// migration's displayed duration to match however long someone happened
// to have the impact checkbox on, not the migration's real duration.
func (s *SQLiteStore) UpdateImpactPeak(ctx context.Context, jobID string, peakSeconds float64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET impact_peak_query_duration_seconds = ? WHERE id = ?
	`, peakSeconds, jobID)
	if err != nil {
		return fmt.Errorf("failed to update impact peak (id=%s): %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrJobNotFound
	}
	return nil
}

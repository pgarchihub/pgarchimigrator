// Package reaper implements Architecture Doc Section 3.3 "Orphan Resource
// Reaper". It is a safety layer independent of, and running BEFORE, the
// Watchdog's "disk 80%" check: it cleans up replication slots and shadow
// tables left orphaned by a crash, network interruption, or user
// cancellation.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/ddlflow"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// DefaultScanInterval is the default scan frequency specified in
// Architecture Doc Section 3.3.
const DefaultScanInterval = 5 * time.Minute

// DefaultStaleThreshold is how long must have passed since an IN_PROGRESS
// job's last update before it is considered "likely orphaned".
const DefaultStaleThreshold = 30 * time.Minute

// ScanResult reports what a single scan pass cleaned up — useful for the
// audit log and for tests.
type ScanResult struct {
	JobsScanned            int
	SlotsDropped           []string
	ShadowTablesDropped    []string
	BackfillIndexesDropped []string
	Errors                 []error
}

// Reaper runs periodic scans and cleans up orphaned resources.
type Reaper struct {
	Store          state.Store
	Pool           *pgxpool.Pool
	ScanInterval   time.Duration
	StaleThreshold time.Duration
}

// New creates a Reaper with the given dependencies.
func New(store state.Store, pool *pgxpool.Pool) *Reaper {
	return &Reaper{
		Store:          store,
		Pool:           pool,
		ScanInterval:   DefaultScanInterval,
		StaleThreshold: DefaultStaleThreshold,
	}
}

// Run runs the periodic scan loop until ctx is cancelled. Typically started
// as a background goroutine from cmd/pgarchimigrator/main.go. Each tick runs both
// ScanOnce (orphan/crash cleanup) and SweepExpiredRollbackWindows
// (completing successful migrations whose FR-08a grace period has ended)
// — these are deliberately separate, non-overlapping concerns (see the
// doc comments on SQLiteStore.ListStale and ListExpiredRollbackWindows).
func (r *Reaper) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.ScanOnce(ctx); err != nil {
				// TODO: log the error via internal/auditlog instead of panicking.
			}
			if _, err := r.SweepExpiredRollbackWindows(ctx); err != nil {
				// TODO: log the error via internal/auditlog instead of panicking.
			}
		}
	}
}

// ScanOnce runs a single scan pass (exported so it can be used by tests and
// a manual "reaper run-once" CLI command):
//  1. Find suspicious jobs via state.Store.ListStale.
//  2. For each job: check if its replication slot still exists in
//     pg_replication_slots, and drop it if so.
//  3. If Job.ShadowTableName is set, check whether the table still exists
//     and drop it if so.
//  4. Mark Job.Phase = state.PhaseAborted.
//
// Trigger cleanup is deliberately out of scope: triggers are dropped along
// with the shadow table via DROP TABLE (the DISABLE/ENABLE handling in
// Section 4.3.1 only applies during an "active" migration; during orphan
// cleanup they go away with the table).
func (r *Reaper) ScanOnce(ctx context.Context) (*ScanResult, error) {
	staleJobs, err := r.Store.ListStale(ctx, r.StaleThreshold)
	if err != nil {
		return nil, fmt.Errorf("failed to list stale jobs: %w", err)
	}

	result := &ScanResult{JobsScanned: len(staleJobs)}

	for _, job := range staleJobs {
		if job.ReplicationSlotName != "" {
			dropped, err := r.dropSlotIfExists(ctx, job.ReplicationSlotName)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to clean up slot: %w", job.ID, err))
			} else if dropped {
				result.SlotsDropped = append(result.SlotsDropped, job.ReplicationSlotName)
			}
		}

		if job.ShadowTableName != "" {
			dropped, err := r.dropShadowTableIfExists(ctx, job.SchemaName, job.ShadowTableName)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to clean up shadow table: %w", job.ID, err))
			} else if dropped {
				result.ShadowTablesDropped = append(result.ShadowTablesDropped, job.ShadowTableName)
			}
		}

		// EXPAND_BACKFILL jobs (internal/ddlflow's ADD_COLUMN/RENAME_COLUMN
		// path) create a temporary partial index for the duration of the
		// backfill loop (see internal/ddlflow's createBackfillIndex doc
		// comment) — normally dropped by a `defer` once the job finishes,
		// but a crash/kill mid-backfill leaves that defer unreached. There's
		// no persisted job field naming the exact index the way
		// ReplicationSlotName/ShadowTableName do (its name is derivable
		// entirely from job.ColumnName + job.ID, so it never needed one) —
		// dropOrphanedBackfillIndexes finds it by pattern instead.
		if job.Strategy == "EXPAND_BACKFILL" {
			dropped, err := r.dropOrphanedBackfillIndexes(ctx, job.SchemaName, job.TableName)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to clean up backfill index: %w", job.ID, err))
			}
			result.BackfillIndexesDropped = append(result.BackfillIndexesDropped, dropped...)
		}

		if err := r.Store.UpdatePhase(ctx, job.ID, state.PhaseAborted); err != nil && !errors.Is(err, state.ErrJobNotFound) {
			result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to mark phase ABORTED: %w", job.ID, err))
		}
	}

	if len(result.Errors) > 0 {
		return result, fmt.Errorf("scan completed with %d error(s)", len(result.Errors))
	}
	return result, nil
}

// SweepResult reports what a single rollback-window sweep cleaned up.
type SweepResult struct {
	JobsSwept int
	Errors    []error
}

// SweepExpiredRollbackWindows completes the Cleanup step (Architecture Doc
// Section 4.1 step 7) for jobs whose FR-08a rollback window has passed
// without a Rollback call. This is conceptually distinct from ScanOnce:
// these jobs represent SUCCESSFUL migrations, not orphaned or crashed
// ones, so they are marked COMPLETED here — never ABORTED.
//
// Two different finalization paths exist, dispatched on job.Operation:
//   - "DROP_COLUMN" (internal/ddlflow's two-phase drop): finalizeDropColumn
//     performs the actual, irreversible ALTER TABLE DROP COLUMN.
//   - everything else (internal/shadowflow's shadow-table strategy): the
//     temp table, replication slot, and publication are dropped;
//     job.ShadowTableName (the *renamed-away* shadow table name that is
//     now live under the original table name — not to be confused with
//     the separate, never-persisted "temp table" that holds the pre-swap
//     data) is left as historical metadata on the job record.
func (r *Reaper) SweepExpiredRollbackWindows(ctx context.Context) (*SweepResult, error) {
	jobs, err := r.Store.ListExpiredRollbackWindows(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list expired rollback windows: %w", err)
	}

	result := &SweepResult{JobsSwept: len(jobs)}

	for _, job := range jobs {
		if job.Operation == "DROP_COLUMN" {
			if err := r.finalizeDropColumn(ctx, job); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to finalize DROP_COLUMN: %w", job.ID, err))
			}
		} else {
			_, tempTable, slotName, pubName := shadowflow.ResourceNames(job.ID, job.TableName)

			dropTempSQL := fmt.Sprintf("DROP TABLE IF EXISTS %s.%s", quoteIdent(job.SchemaName), quoteIdent(tempTable))
			if err := execDDLWithLockTimeout(ctx, r.Pool, dropTempSQL); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to drop temp table: %w", job.ID, err))
			}

			if _, err := r.dropSlotIfExists(ctx, slotName); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to clean up replication slot: %w", job.ID, err))
			}

			if err := shadowflow.DropPublicationIfExists(ctx, r.Pool, pubName); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to clean up publication: %w", job.ID, err))
			}
		}

		if err := r.Store.UpdatePhase(ctx, job.ID, state.PhaseCompleted); err != nil && !errors.Is(err, state.ErrJobNotFound) {
			result.Errors = append(result.Errors, fmt.Errorf("job %s: failed to mark phase COMPLETED: %w", job.ID, err))
		}
	}

	if len(result.Errors) > 0 {
		return result, fmt.Errorf("sweep completed with %d error(s)", len(result.Errors))
	}
	return result, nil
}

// finalizeDropColumn performs the actual, irreversible
// ALTER TABLE ... DROP COLUMN for a DROP_COLUMN job whose rollback window
// has expired without the operator calling Rollback. This is the second
// phase of internal/ddlflow's two-phase drop (see
// DDLFlow.executeDropColumn) — only once this runs is the column's data
// actually gone.
//
// Defense in depth: this only proceeds if job.DeprecatedColumnName is
// both non-empty AND carries the expected naming convention prefix (see
// ddlflow.DeprecatedColumnPrefix) — a sanity check against ever dropping
// the wrong column due to a corrupted or unexpected state record.
func (r *Reaper) finalizeDropColumn(ctx context.Context, job *state.Job) error {
	if job.DeprecatedColumnName == "" {
		return fmt.Errorf("job has no recorded deprecated column name — refusing to guess what to drop")
	}
	if !strings.HasPrefix(job.DeprecatedColumnName, ddlflow.DeprecatedColumnPrefix) {
		return fmt.Errorf("deprecated column name %q does not match the expected naming convention — refusing to drop it", job.DeprecatedColumnName)
	}

	ddl := fmt.Sprintf("ALTER TABLE %s.%s DROP COLUMN IF EXISTS %s",
		quoteIdent(job.SchemaName), quoteIdent(job.TableName), quoteIdent(job.DeprecatedColumnName))
	if err := execDDLWithLockTimeout(ctx, r.Pool, ddl); err != nil {
		return fmt.Errorf("failed to drop deprecated column %q: %w", job.DeprecatedColumnName, err)
	}
	return nil
}

// dropSlotIfExists checks whether the replication slot still exists and,
// if so, drops it safely. If the slot is actively in use (held by a
// connection), pg_drop_replication_slot fails — in that case we propagate
// the error as-is and do not force termination (data safety takes priority).
func (r *Reaper) dropSlotIfExists(ctx context.Context, slotName string) (bool, error) {
	var exists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`
	if err := r.Pool.QueryRow(ctx, checkQuery, slotName).Scan(&exists); err != nil {
		return false, fmt.Errorf("failed to check slot existence: %w", err)
	}
	if !exists {
		return false, nil
	}

	if _, err := r.Pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slotName); err != nil {
		return false, fmt.Errorf("failed to drop slot (likely still in active use): %w", err)
	}
	return true, nil
}

// dropShadowTableIfExists checks whether the shadow table matching the
// naming convention still exists and drops it if so. to_regclass returns
// NULL instead of erroring when the table doesn't exist — this is why it
// was chosen over a pg_constraint/regclass cast (a similar, "soft"
// existence check as the fix applied in preflight.go).
func (r *Reaper) dropShadowTableIfExists(ctx context.Context, schema, table string) (bool, error) {
	qualifiedName := fmt.Sprintf("%s.%s", schema, table)

	var regclassResult *string
	checkQuery := `SELECT to_regclass($1)::text`
	if err := r.Pool.QueryRow(ctx, checkQuery, qualifiedName).Scan(&regclassResult); err != nil {
		return false, fmt.Errorf("failed to check shadow table existence: %w", err)
	}
	if regclassResult == nil {
		return false, nil // table no longer exists
	}

	dropQuery := fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s`, quoteIdent(schema), quoteIdent(table))
	if err := execDDLWithLockTimeout(ctx, r.Pool, dropQuery); err != nil {
		return false, fmt.Errorf("failed to drop shadow table: %w", err)
	}
	return true, nil
}

// dropOrphanedBackfillIndexes finds and drops any temporary partial
// index (see internal/ddlflow's createBackfillIndex doc comment) left
// behind on the given table — normally cleaned up by a `defer` in
// internal/ddlflow's executeExpandBackfill/executeRenameColumn, reachable
// here only when a crash/kill interrupted a backfill before that defer
// ran. Matched by ddlflow.BackfillIndexPrefix rather than a job-specific
// exact name: unlike ReplicationSlotName/ShadowTableName, the backfill
// index's exact name was never persisted on the job record (it's fully
// derivable from job.ColumnName + job.ID, so it never needed a dedicated
// field) — a prefix-scoped lookup on this specific table finds it without
// needing to reconstruct that exact name here. Drops every match found
// (plural, unlike the singular slot/shadow-table cleanups above) since
// more than one crashed attempt over time could each have left one
// behind, and there's no reason to stop after the first.
func (r *Reaper) dropOrphanedBackfillIndexes(ctx context.Context, schema, table string) ([]string, error) {
	rows, err := r.Pool.Query(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2 AND indexname LIKE $3
	`, schema, table, ddlflow.BackfillIndexPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to look up orphaned backfill indexes: %w", err)
	}
	var indexNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan index name: %w", err)
		}
		indexNames = append(indexNames, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var dropped []string
	var firstErr error
	for _, name := range indexNames {
		// Deliberately a plain Pool.Exec, NOT execDDLWithLockTimeout —
		// the latter wraps its statement in an explicit transaction
		// (needed for SET LOCAL lock_timeout), but CREATE/DROP INDEX
		// CONCURRENTLY can NEVER run inside any transaction block at
		// all, a hard PostgreSQL restriction (SQLSTATE 25001) — mixing
		// the two is a real bug this project already avoided everywhere
		// else (see ddlflow.go's own dropIndexConcurrently, which uses
		// this exact same plain-Exec pattern for the identical reason),
		// caught here by an integration test actually exercising this
		// function against a real PostgreSQL instance rather than a
		// mock.
		dropQuery := fmt.Sprintf(`DROP INDEX CONCURRENTLY IF EXISTS %s.%s`, quoteIdent(schema), quoteIdent(name))
		if _, err := r.Pool.Exec(ctx, dropQuery); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to drop backfill index %s: %w", name, err)
			}
			continue // best-effort: still try the rest even if one fails
		}
		dropped = append(dropped, name)
	}
	return dropped, firstErr
}

// quoteIdent is a simple SQL-injection guard for DDL statements where
// identifiers cannot be bound as parameters. Schema and table names come
// from internal/state via our own naming convention (__pgam_shadow_*), so
// the risk is low, but defensive escaping of double quotes is applied
// anyway.
// ddlLockTimeout/ddlLockTimeoutMaxAttempts and execDDLWithLockTimeout
// mirror internal/ddlflow's identically-named helper — see that
// package's doc comment for the full "why" (a real 10M-row load test
// found this: even a fast, metadata-only ALTER TABLE can end up queued
// behind slow transactions and, via PostgreSQL's own FIFO/non-starving
// lock queue, block every OTHER query on the table behind it too). The
// reaper's own DDL (finalizing a DROP_COLUMN once its rollback window
// expires, cleaning up an orphaned shadow table) runs on a background
// timer rather than in response to a user action, but it targets the
// SAME live, actively-queried tables — an unprotected lock wait here
// causes the identical latency spike for real application traffic, just
// triggered by a background sweep instead of a foreground API call.
//
// Duplicated here rather than exported from internal/ddlflow: this
// project's established convention (see quoteIdent's several independent
// copies across internal/ddlflow, internal/shadowflow, internal/preview,
// internal/progress) is a small, self-contained per-package helper over
// a new shared dependency for something this size — see the security
// audit's note on this trade-off for the case in favor of eventually
// consolidating it into a shared package.
const (
	ddlLockTimeout            = 3 * time.Second
	ddlLockTimeoutMaxAttempts = 5
)

func execDDLWithLockTimeout(ctx context.Context, pool *pgxpool.Pool, ddl string) error {
	var lastErr error
	for attempt := 1; attempt <= ddlLockTimeoutMaxAttempts; attempt++ {
		err := execOnceWithLockTimeout(ctx, pool, ddl)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isLockTimeoutError(err) {
			return err
		}
		if attempt == ddlLockTimeoutMaxAttempts {
			break
		}
		backoff := time.Duration(attempt) * 500 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("gave up after %d attempts, each blocked by a conflicting lock (lock_timeout=%s): %w",
		ddlLockTimeoutMaxAttempts, ddlLockTimeout, lastErr)
}

func execOnceWithLockTimeout(ctx context.Context, pool *pgxpool.Pool, ddl string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%s'", ddlLockTimeout)); err != nil {
		return fmt.Errorf("failed to set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isLockTimeoutError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return false
}

func quoteIdent(ident string) string {
	escaped := ""
	for _, r := range ident {
		if r == '"' {
			escaped += `""`
		} else {
			escaped += string(r)
		}
	}
	return `"` + escaped + `"`
}

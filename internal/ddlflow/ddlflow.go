// Package ddlflow implements the "Direct DDL" and "Expand & Backfill" rows
// of the Architecture Doc Section 4.0 decision matrix. It does not require
// the shadow-table / WAL flow; see internal/shadowflow for that.
//
// Supported operations: ADD_COLUMN, DROP_COLUMN, ADD_INDEX, DROP_INDEX,
// SET_NOT_NULL, ADD_CONSTRAINT, RENAME_COLUMN.
package ddlflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/monitor"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// defaultBatchSize is the recommended batch size against Architecture Doc
// Section 8 "Long-Running Transaction" risk ("commit every 10k rows").
const defaultBatchSize = 10000

// lockWaitBackoff is the simple wait applied when a lock wait is detected
// during the backfill loop. TODO: exponential backoff + max attempts (could
// be aligned with the mechanism in swap.go).
const lockWaitBackoff = 2 * time.Second

// defaultDropColumnRollbackWindow mirrors internal/shadowflow's FR-08a
// grace period (Architecture Doc default: 10 minutes) — this is the
// window during which a DROP_COLUMN soft-drop can still be reversed
// before the reaper finalizes it (see executeDropColumn's doc comment).
const defaultDropColumnRollbackWindow = 10 * time.Minute

// DeprecatedColumnPrefix marks a column as being in DROP_COLUMN's
// soft-drop state — internal/reaper's finalizeDropColumn identifies
// columns to actually remove by this prefix as a defense-in-depth check
// against the persisted DeprecatedColumnName field alone. Exported so
// internal/reaper can perform that same sanity check without duplicating
// the literal string.
const DeprecatedColumnPrefix = "__pgam_dropped_"

// BackfillIndexPrefix marks a temporary, backfill-only partial index —
// see executeExpandBackfill's doc comment for why this index exists at
// all. Exported for the same reason as DeprecatedColumnPrefix: so
// internal/reaper can recognize and clean up an orphaned one (left behind
// by a crashed/interrupted migration) without duplicating the literal
// string.
const BackfillIndexPrefix = "__pgam_backfill_idx_"

// DDLFlow implements the orchestrator.Flow interface.
type DDLFlow struct {
	Pool                     *pgxpool.Pool
	Store                    state.Store
	Watcher                  monitor.Watcher // optional; for FR-05/FR-06 awareness (nil skips throttling)
	BatchSize                int
	DropColumnRollbackWindow time.Duration // optional; defaults to defaultDropColumnRollbackWindow (10 min)
}

var _ orchestrator.Flow = (*DDLFlow)(nil)

// New creates a DDLFlow with the given dependencies.
func New(pool *pgxpool.Pool, store state.Store) *DDLFlow {
	return &DDLFlow{Pool: pool, Store: store, BatchSize: defaultBatchSize}
}

// Execute runs one of two sub-scenarios based on job.Operation and
// job.IsVolatileDefault (Architecture Doc 4.0 rows 1-2):
//   - Fixed/no default -> executeDirectAddColumn (metadata-only, PG 11+)
//   - Volatile default -> executeExpandBackfill (add as NULL, backfill in the background)
func (f *DDLFlow) Execute(ctx context.Context, job *state.Job) error {
	switch job.Operation {
	case "ADD_COLUMN":
		if job.IsVolatileDefault {
			return f.executeExpandBackfill(ctx, job)
		}
		return f.executeDirectAddColumn(ctx, job)
	case "DROP_COLUMN":
		return f.executeDropColumn(ctx, job)
	case "ADD_INDEX":
		return f.executeAddIndex(ctx, job)
	case "DROP_INDEX":
		return f.executeDropIndex(ctx, job)
	case "SET_NOT_NULL":
		return f.executeSetNotNull(ctx, job)
	case "ADD_CONSTRAINT":
		return f.executeAddConstraint(ctx, job)
	case "RENAME_COLUMN":
		return f.executeRenameColumn(ctx, job)
	default:
		return fmt.Errorf("ddlflow: unsupported operation: %s (supported: ADD_COLUMN, DROP_COLUMN, ADD_INDEX, DROP_INDEX, SET_NOT_NULL, ADD_CONSTRAINT, RENAME_COLUMN)", job.Operation)
	}
}

// executeDirectAddColumn implements Section 4.0 row 1: ADD COLUMN with a
// fixed/no default. A single DDL statement is enough since it's
// metadata-only on PostgreSQL 11+.
func (f *DDLFlow) executeDirectAddColumn(ctx context.Context, job *state.Job) error {
	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	// Defense in depth — see strategy.ValidateColumnType/ValidateSQLExpression's
	// own doc comments for the real injection vector this closes.
	// internal/orchestrator.StartMigration already validates these
	// before a job even exists; this second check protects any DIRECT
	// caller of this flow too (tests, a future non-HTTP entry point),
	// not just ones that went through StartMigration.
	if err := strategy.ValidateColumnType(job.ColumnType); err != nil {
		return f.fail(ctx, job, err)
	}
	if job.DefaultValue != "" {
		if err := strategy.ValidateSQLExpression(job.DefaultValue, "default value"); err != nil {
			return f.fail(ctx, job, err)
		}
	}

	ddl := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
		qualifiedTable(job), quoteIdent(job.ColumnName), job.ColumnType)
	if job.DefaultValue != "" {
		// NOTE: parameters cannot be bound in DDL statements; DefaultValue is
		// inlined into the SQL literally here — validated just above.
		ddl += " DEFAULT " + job.DefaultValue
	}

	if err := execDDLWithLockTimeout(ctx, f.Pool, ddl); err != nil {
		return f.fail(ctx, job, fmt.Errorf("ALTER TABLE failed: %w", err))
	}

	return f.setPhase(ctx, job, state.PhaseCompleted)
}

// executeExpandBackfill implements Section 4.0 row 2: ADD COLUMN with a
// volatile default.
// 1) The column is added as NULL (metadata-only, fast).
// 2) Backfilled in the background using ctid-based batches (TR-09: dynamically adjustable).
// 3) Validation: the count of remaining NULL rows must be 0.
// backfillIndexName deterministically derives a temporary partial
// index's name from the job — see createBackfillIndex's doc comment for
// what this index is for. Includes a short slice of the job ID so two
// backfills on the same table/column (e.g. a retry after a failure)
// never collide.
func backfillIndexName(job *state.Job) string {
	shortID := job.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return fmt.Sprintf("%s%s_%s", BackfillIndexPrefix, job.ColumnName, shortID)
}

// createBackfillIndex builds a temporary partial index — CREATE INDEX
// CONCURRENTLY ... WHERE <whereClause> — used ONLY to keep the backfill
// loop's own "WHERE <whereClause> LIMIT N" queries fast regardless of
// table size or how far the backfill has already progressed. The caller
// is responsible for dropping it once the backfill finishes (see
// executeExpandBackfill/executeRenameColumn's defer).
//
// Why this exists — found via a real 10M-row load test, in two stages:
//
// Stage 1: without ANY progress tracking, "WHERE col IS NULL LIMIT N"
// re-scans from the start of the table's physical layout every batch,
// getting progressively more expensive as more rows are already
// filled — effectively an O(n²) cost over the whole backfill, and each
// increasingly-slow batch statement holds row-level locks for its entire
// duration, blocking concurrent application queries touching the same
// rows for as long as 33 seconds in one measured run.
//
// Stage 2 (this fix supersedes an earlier attempt): a ctid-based cursor
// ("WHERE ctid > $lastCtid ORDER BY ctid LIMIT N") was tried first,
// reasoning that ctid's natural physical ordering would let PostgreSQL
// use an efficient TID Range Scan to pick up exactly where the previous
// batch left off. EXPLAIN ANALYZE against a real PostgreSQL 16 instance
// showed this reasoning didn't hold in practice: the planner chose a
// Parallel Seq Scan instead (not a TID Range Scan), because ORDER BY
// ctid combined with the additional "col IS NULL" filter defeated the
// range-scan-eligible plan shape — reading nearly the entire table on
// every batch anyway. Measured stalls reached 63 SECONDS, worse than the
// original unbounded-scan version this was meant to fix.
//
// A partial index sidesteps the query-plan uncertainty entirely: its
// only entries are rows matching whereClause, so "WHERE <whereClause>
// LIMIT N" becomes a plain, fast Index Scan no matter what the planner
// would otherwise choose — and because PostgreSQL automatically removes
// a row from a partial index once it no longer satisfies the index's own
// WHERE clause, the index shrinks as the backfill fills in more rows,
// staying fast for the same reason throughout the whole backfill instead
// of degrading. This is the standard, well-established technique for
// exactly this problem (used by several production "safe migration"
// libraries) — unlike the ctid cursor, it doesn't depend on the query
// planner making a particular choice.
//
// CREATE INDEX CONCURRENTLY can leave an INVALID index behind if
// interrupted (a known PostgreSQL failure mode) — checked and cleaned up
// here the same way executeAddIndex already handles it.
func (f *DDLFlow) createBackfillIndex(ctx context.Context, job *state.Job, indexName, indexedColumn, whereClause string) error {
	ddl := fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s) WHERE %s",
		quoteIdent(indexName), qualifiedTable(job), quoteIdent(indexedColumn), whereClause)
	if _, err := f.Pool.Exec(ctx, ddl); err != nil {
		_ = f.dropIndexConcurrently(ctx, job.SchemaName, indexName)
		return fmt.Errorf("CREATE INDEX CONCURRENTLY failed: %w", err)
	}

	valid, err := f.isIndexValid(ctx, job.SchemaName, indexName)
	if err != nil {
		return fmt.Errorf("failed to verify backfill index validity: %w", err)
	}
	if !valid {
		_ = f.dropIndexConcurrently(ctx, job.SchemaName, indexName)
		return fmt.Errorf(
			"CREATE INDEX CONCURRENTLY completed but produced an INVALID index — a known PostgreSQL failure mode; " +
				"the invalid index has been dropped, retry the migration")
	}
	return nil
}

// dropBackfillIndexBestEffort is meant for a `defer` right after a
// successful createBackfillIndex — dropping the temporary index is a
// cleanup convenience (avoids leaving a stale index around after a
// successful migration, and avoids a retry after a FAILURE colliding
// with CREATE INDEX CONCURRENTLY IF NOT EXISTS finding a
// mismatched/invalid old one), not correctness-critical: a failure here
// doesn't change whether the migration itself succeeded or failed, and
// internal/reaper independently sweeps up any orphan this leaves behind
// (e.g. if the process crashes before this defer runs). Deliberately
// takes a fresh context.Background() rather than the caller's ctx — if
// the caller's own context was what's cancelled/timed out, cleanup
// should still be attempted rather than immediately giving up too.
func (f *DDLFlow) dropBackfillIndexBestEffort(schema, indexName string) {
	if err := f.dropIndexConcurrently(context.Background(), schema, indexName); err != nil {
		log.Printf("ddlflow: failed to drop temporary backfill index %s.%s: %v", schema, indexName, err)
	}
}

func (f *DDLFlow) executeExpandBackfill(ctx context.Context, job *state.Job) error {
	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	// Defense in depth — see executeDirectAddColumn's identical check
	// just above for why this exists at every DDL-building call site,
	// not just internal/orchestrator's earlier one.
	if err := strategy.ValidateColumnType(job.ColumnType); err != nil {
		return f.fail(ctx, job, err)
	}
	if job.DefaultValue != "" {
		if err := strategy.ValidateSQLExpression(job.DefaultValue, "default value"); err != nil {
			return f.fail(ctx, job, err)
		}
	}

	addDDL := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
		qualifiedTable(job), quoteIdent(job.ColumnName), job.ColumnType)
	if err := execDDLWithLockTimeout(ctx, f.Pool, addDDL); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to add column: %w", err))
	}

	// See createBackfillIndex's doc comment for why this temporary index
	// exists at all — without it, the backfill loop below degrades badly
	// at scale (a real, load-test-found issue).
	indexName := backfillIndexName(job)
	whereClause := quoteIdent(job.ColumnName) + " IS NULL"
	if err := f.createBackfillIndex(ctx, job, indexName, job.ColumnName, whereClause); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to create backfill index: %w", err))
	}
	defer f.dropBackfillIndexBestEffort(job.SchemaName, indexName)

	if err := f.setPhase(ctx, job, state.PhaseSyncing); err != nil {
		return err
	}

	if err := f.backfillLoop(ctx, job); err != nil {
		return f.fail(ctx, job, err)
	}

	if err := f.setPhase(ctx, job, state.PhaseValidating); err != nil {
		return err
	}

	remaining, err := f.countRemainingNulls(ctx, job)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("validation query failed: %w", err))
	}
	if remaining > 0 {
		return f.fail(ctx, job, fmt.Errorf("backfill incomplete: %d row(s) still NULL", remaining))
	}

	return f.setPhase(ctx, job, state.PhaseCompleted)
}

// deprecatedColumnName builds the temporary name a column is renamed to
// during executeDropColumn's soft-drop step. Includes a short slice of the
// job ID so two DROP_COLUMN jobs on the same table/column (e.g. a retry
// after a failure) never collide.
func deprecatedColumnName(job *state.Job) string {
	shortID := job.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return fmt.Sprintf("%s%s_%s", DeprecatedColumnPrefix, job.ColumnName, shortID)
}

// executeDropColumn implements a two-phase DROP_COLUMN: an immediate,
// fully reversible "soft drop" followed by a rollback window, mirroring
// the FR-08a pattern internal/shadowflow already uses for the
// shadow-table strategy — reusing the same state.PhaseRollbackWindow and
// RollbackDeadline mechanism rather than inventing a parallel one.
//
// The soft drop is a plain RENAME COLUMN (metadata-only, instant,
// trivially reversible) rather than simply leaving the column in place —
// this is deliberate: renaming makes the column genuinely unreachable
// under its original name immediately, so any application code still
// querying it gets a clear "column does not exist" error right away. That
// early, loud failure is the intended forcing function: it surfaces
// still-in-use references BEFORE the rollback window closes and the data
// is irreversibly deleted, rather than after.
//
// Once the window expires without a Rollback call, internal/reaper's
// finalizeDropColumn performs the actual, irreversible
// ALTER TABLE ... DROP COLUMN.
func (f *DDLFlow) executeDropColumn(ctx context.Context, job *state.Job) error {
	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	deprecatedName := deprecatedColumnName(job)
	ddl := fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
		qualifiedTable(job), quoteIdent(job.ColumnName), quoteIdent(deprecatedName))
	if err := execDDLWithLockTimeout(ctx, f.Pool, ddl); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to soft-drop (rename) column: %w", err))
	}

	if err := f.Store.UpdateDeprecatedColumnName(ctx, job.ID, deprecatedName); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to persist the deprecated column name: %w", err))
	}
	job.DeprecatedColumnName = deprecatedName

	deadline := time.Now().UTC().Add(f.dropColumnRollbackWindow())
	if err := f.Store.UpdateRollbackDeadline(ctx, job.ID, deadline); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to persist rollback deadline: %w", err))
	}
	job.RollbackDeadline = &deadline

	return f.setPhase(ctx, job, state.PhaseRollbackWindow)
}

func (f *DDLFlow) dropColumnRollbackWindow() time.Duration {
	if f.DropColumnRollbackWindow <= 0 {
		return defaultDropColumnRollbackWindow
	}
	return f.DropColumnRollbackWindow
}

// defaultIndexName builds a fallback name when the caller doesn't supply
// one for ADD_INDEX. Follows the common "idx_<table>_<column>" convention.
func defaultIndexName(job *state.Job) string {
	return fmt.Sprintf("idx_%s_%s", job.TableName, job.ColumnName)
}

// executeAddIndex implements ADD_INDEX using PostgreSQL's own
// CREATE INDEX CONCURRENTLY — PostgreSQL's native zero-downtime mechanism
// for index builds, so no shadow-table or backfill machinery is needed
// (see strategy.OpAddIndex's doc comment).
//
// KNOWN POSTGRESQL GOTCHA handled here: if CREATE INDEX CONCURRENTLY is
// interrupted or fails partway through (a lock conflict, a constraint
// violation discovered during the build, the connection dropping, etc.),
// PostgreSQL does NOT automatically clean up — it leaves an INVALID index
// behind (pg_index.indisvalid = false). An invalid index is silently
// ignored by the query planner but still occupies disk space AND blocks
// creating another index with the same name. This function checks
// indisvalid after the build and, on either an error or an invalid
// result, drops the broken index before returning — so a retry never
// collides with a leftover from the previous attempt.
//
// CREATE INDEX CONCURRENTLY cannot run inside a transaction block;
// pgxpool.Exec runs each statement in its own implicit (autocommit)
// transaction, so a single Exec call here is correct as-is.
func (f *DDLFlow) executeAddIndex(ctx context.Context, job *state.Job) error {
	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	indexName := job.IndexName
	if indexName == "" {
		indexName = defaultIndexName(job)
	}

	ddl := fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s)",
		quoteIdent(indexName), qualifiedTable(job), quoteIdent(job.ColumnName))
	if _, err := f.Pool.Exec(ctx, ddl); err != nil {
		_ = f.dropIndexConcurrently(ctx, job.SchemaName, indexName) // best-effort: clean up a possible invalid leftover
		return f.fail(ctx, job, fmt.Errorf("CREATE INDEX CONCURRENTLY failed: %w", err))
	}

	// A distinct, visible phase for this check — see pipelineFor's own
	// comment (internal/progress) for why: the check itself already
	// existed and already failed the job on an invalid index, but that
	// enforcement was previously invisible, folded silently into the
	// same "Preparation" phase as everything else. Genuinely
	// transitioning through PhaseValidating here (not just checking
	// isIndexValid inline) is what makes this show up as its own step
	// in the Migration Detail page's step list and Health Card.
	if err := f.setPhase(ctx, job, state.PhaseValidating); err != nil {
		return err
	}

	valid, err := f.isIndexValid(ctx, job.SchemaName, indexName)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to verify index validity: %w", err))
	}
	if !valid {
		_ = f.dropIndexConcurrently(ctx, job.SchemaName, indexName)
		return f.fail(ctx, job, fmt.Errorf(
			"CREATE INDEX CONCURRENTLY completed but produced an INVALID index — a known PostgreSQL failure mode; "+
				"the invalid index %q has been dropped, retry the migration", indexName))
	}

	if err := f.Store.UpdateIndexName(ctx, job.ID, indexName); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to persist index name: %w", err))
	}
	job.IndexName = indexName

	return f.setPhase(ctx, job, state.PhaseCompleted)
}

// executeDropIndex implements DROP_INDEX using DROP INDEX CONCURRENTLY
// (PostgreSQL's non-blocking index drop, available since PG 9.6).
//
// Unlike DROP_COLUMN, this needs no time-boxed rollback window: dropping
// an index destroys no data and breaks no application queries (only their
// performance) — it is always safe to recreate later. Rollback therefore
// works at ANY time, even long after COMPLETED, as long as the
// pg_get_indexdef() definition captured here (BEFORE the drop) is still on
// the job record — see DDLFlow.rollbackDropIndex.
func (f *DDLFlow) executeDropIndex(ctx context.Context, job *state.Job) error {
	if job.IndexName == "" {
		return f.fail(ctx, job, fmt.Errorf("DROP_INDEX requires an index name"))
	}
	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	definition, err := f.captureIndexDefinition(ctx, job.SchemaName, job.IndexName)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to capture index definition before drop: %w", err))
	}
	if err := f.Store.UpdateIndexDefinition(ctx, job.ID, definition); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to persist captured index definition: %w", err))
	}
	job.IndexDefinition = definition

	if err := f.dropIndexConcurrently(ctx, job.SchemaName, job.IndexName); err != nil {
		return f.fail(ctx, job, fmt.Errorf("DROP INDEX CONCURRENTLY failed: %w", err))
	}

	return f.setPhase(ctx, job, state.PhaseCompleted)
}

func (f *DDLFlow) dropIndexConcurrently(ctx context.Context, schema, indexName string) error {
	ddl := fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s.%s", quoteIdent(schema), quoteIdent(indexName))
	_, err := f.Pool.Exec(ctx, ddl)
	return err
}

func (f *DDLFlow) isIndexValid(ctx context.Context, schema, indexName string) (bool, error) {
	var valid bool
	query := `
		SELECT i.indisvalid
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
	`
	err := f.Pool.QueryRow(ctx, query, schema, indexName).Scan(&valid)
	return valid, err
}

// captureIndexDefinition reads the exact CREATE INDEX statement that would
// recreate the given index, via pg_get_indexdef() — this is what makes
// rollbackDropIndex able to recreate an identical index later.
func (f *DDLFlow) captureIndexDefinition(ctx context.Context, schema, indexName string) (string, error) {
	var def string
	query := `
		SELECT pg_get_indexdef(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
	`
	err := f.Pool.QueryRow(ctx, query, schema, indexName).Scan(&def)
	if err != nil {
		return "", fmt.Errorf("index %q not found in schema %q: %w", indexName, schema, err)
	}
	return def, nil
}

// makeConcurrent inserts CONCURRENTLY into a captured pg_get_indexdef()
// definition. pg_get_indexdef never includes CONCURRENTLY itself — it's a
// build-time option, not part of an index's persisted definition — so a
// captured definition must have it re-inserted before being replayed, or
// the replay would take a blocking lock.
func makeConcurrent(createIndexDDL string) string {
	if strings.HasPrefix(createIndexDDL, "CREATE UNIQUE INDEX ") {
		return "CREATE UNIQUE INDEX CONCURRENTLY " + strings.TrimPrefix(createIndexDDL, "CREATE UNIQUE INDEX ")
	}
	return "CREATE INDEX CONCURRENTLY " + strings.TrimPrefix(createIndexDDL, "CREATE INDEX ")
}

// executeSetNotNull implements SET_NOT_NULL via PostgreSQL's own
// non-blocking "expand and validate" pattern (available since PG 12): a
// plain ALTER TABLE ... ALTER COLUMN ... SET NOT NULL must scan the whole
// table to verify no existing row is NULL, and it holds an ACCESS
// EXCLUSIVE lock for that entire scan — exactly the kind of blocking
// operation this tool exists to avoid. Instead:
//  1. Add a CHECK (col IS NOT NULL) constraint NOT VALID — instant,
//     metadata-only, since NOT VALID skips the verification scan.
//  2. VALIDATE CONSTRAINT separately — this does the same full-table scan,
//     but under a SHARE UPDATE EXCLUSIVE lock, which doesn't block
//     concurrent reads or writes while it runs.
//  3. Only now run SET NOT NULL: PostgreSQL's planner recognizes the
//     already-validated constraint proves the invariant, so this
//     completes near-instantly rather than re-scanning.
//  4. Drop the now-redundant CHECK constraint — the column's own NOT NULL
//     flag enforces the same rule going forward with less overhead.
func (f *DDLFlow) executeSetNotNull(ctx context.Context, job *state.Job) error {
	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	constraintName := job.ConstraintName
	if constraintName == "" {
		constraintName = fmt.Sprintf("%s_%s_not_null_check", job.TableName, job.ColumnName)
	}

	addDDL := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID",
		qualifiedTable(job), quoteIdent(constraintName), quoteIdent(job.ColumnName))
	if err := execDDLWithLockTimeout(ctx, f.Pool, addDDL); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to add the (not yet validated) NOT NULL check constraint: %w", err))
	}
	if err := f.Store.UpdateConstraintName(ctx, job.ID, constraintName); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to persist constraint name: %w", err))
	}
	job.ConstraintName = constraintName

	if err := f.setPhase(ctx, job, state.PhaseValidating); err != nil {
		return err
	}

	validateDDL := fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", qualifiedTable(job), quoteIdent(constraintName))
	if _, err := f.Pool.Exec(ctx, validateDDL); err != nil {
		// Validation failed — almost certainly because an existing row IS
		// NULL. Drop the broken constraint rather than leaving a
		// permanently-invalid one behind (same defense-in-depth spirit as
		// executeAddIndex's invalid-index cleanup).
		_, _ = f.Pool.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", qualifiedTable(job), quoteIdent(constraintName)))
		return f.fail(ctx, job, fmt.Errorf("constraint validation failed (likely an existing NULL value in %s): %w", job.ColumnName, err))
	}

	setNotNullDDL := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", qualifiedTable(job), quoteIdent(job.ColumnName))
	if _, err := f.Pool.Exec(ctx, setNotNullDDL); err != nil {
		return f.fail(ctx, job, fmt.Errorf("SET NOT NULL failed even after successful validation (unexpected): %w", err))
	}

	dropCheckDDL := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", qualifiedTable(job), quoteIdent(constraintName))
	if _, err := f.Pool.Exec(ctx, dropCheckDDL); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to drop the now-redundant temporary check constraint: %w", err))
	}

	return f.setPhase(ctx, job, state.PhaseCompleted)
}

// executeAddConstraint implements ADD_CONSTRAINT using the same NOT VALID
// + VALIDATE CONSTRAINT pattern as executeSetNotNull, but for an
// arbitrary, user-supplied CHECK expression kept as a genuine, permanent,
// named constraint (not converted into a column property the way
// SET_NOT_NULL is).
func (f *DDLFlow) executeAddConstraint(ctx context.Context, job *state.Job) error {
	if job.ConstraintName == "" {
		return f.fail(ctx, job, fmt.Errorf("ADD_CONSTRAINT requires a constraint name"))
	}
	if job.CheckExpression == "" {
		return f.fail(ctx, job, fmt.Errorf("ADD_CONSTRAINT requires a check expression"))
	}

	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	// Defense in depth — see strategy.ValidateSQLExpression's own doc
	// comment for the real injection vector this closes.
	// internal/orchestrator.StartMigration already validates this
	// before a job even exists; this second check protects any DIRECT
	// caller of this flow too, matching executeDirectAddColumn/
	// executeExpandBackfill's identical checks above for
	// DefaultValue/ColumnType.
	if err := strategy.ValidateSQLExpression(job.CheckExpression, "check expression"); err != nil {
		return f.fail(ctx, job, err)
	}

	// NOTE: like DefaultValue elsewhere in this package, CheckExpression
	// cannot be parameter-bound in DDL and is inlined into the SQL
	// literally here — validated just above.
	addDDL := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID",
		qualifiedTable(job), quoteIdent(job.ConstraintName), job.CheckExpression)
	if err := execDDLWithLockTimeout(ctx, f.Pool, addDDL); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to add the (not yet validated) check constraint: %w", err))
	}

	if err := f.setPhase(ctx, job, state.PhaseValidating); err != nil {
		return err
	}

	validateDDL := fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", qualifiedTable(job), quoteIdent(job.ConstraintName))
	if _, err := f.Pool.Exec(ctx, validateDDL); err != nil {
		_, _ = f.Pool.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", qualifiedTable(job), quoteIdent(job.ConstraintName)))
		return f.fail(ctx, job, fmt.Errorf("constraint validation failed (an existing row violates it): %w", err))
	}

	return f.setPhase(ctx, job, state.PhaseCompleted)
}

// executeRenameColumn implements RENAME_COLUMN via a real expand &
// contract pattern, NOT a plain ALTER TABLE ... RENAME COLUMN. That
// statement is metadata-only and instant at the database level, but it
// breaks any application code still using the old name THE MOMENT it
// runs — the downtime just moves from the database to every caller that
// hasn't been redeployed yet, which defeats the point of this whole tool.
//
// Instead:
//  1. Add a new column under the new name (same type as the old one).
//  2. Create a BEFORE INSERT OR UPDATE trigger that keeps both columns in
//     sync: whichever one a statement actually writes propagates to the
//     other. Old application code (writing only the old name) keeps
//     working exactly as before; new application code (writing the new
//     name, once redeployed) works too; either is always readable via
//     both names.
//  3. Backfill the new column from the old one for pre-existing rows.
//  4. Validate the backfill is complete.
//
// The job then sits in a "dual-write" COMPLETED state — deliberately NOT
// a final state the way COMPLETED means for other operations. Both
// columns and the sync trigger stay in place indefinitely; finishing the
// rename (dropping the old column and this trigger) is a deliberate,
// separate, LATER migration — run DROP_COLUMN on the old name once the
// application has been redeployed to use the new one exclusively and
// that's been confirmed safe. This operation's only job is getting to
// that safe dual-write state, not deciding when it's over.
func (f *DDLFlow) executeRenameColumn(ctx context.Context, job *state.Job) error {
	if job.NewColumnName == "" {
		return f.fail(ctx, job, fmt.Errorf("RENAME_COLUMN requires a new column name"))
	}

	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}

	colType, err := f.columnType(ctx, job.SchemaName, job.TableName, job.ColumnName)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to determine the existing column's type: %w", err))
	}

	addDDL := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
		qualifiedTable(job), quoteIdent(job.NewColumnName), colType)
	if err := execDDLWithLockTimeout(ctx, f.Pool, addDDL); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to add the new column: %w", err))
	}

	if err := f.createRenameSyncTrigger(ctx, job); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to create the dual-write sync trigger: %w", err))
	}

	// See createBackfillIndex's doc comment for why this exists — same
	// reasoning as executeExpandBackfill's identical index, just matching
	// RENAME_COLUMN's two-part backfill predicate instead of a plain
	// "col IS NULL".
	indexName := backfillIndexName(job)
	whereClause := fmt.Sprintf("%s IS NULL AND %s IS NOT NULL", quoteIdent(job.NewColumnName), quoteIdent(job.ColumnName))
	if err := f.createBackfillIndex(ctx, job, indexName, job.NewColumnName, whereClause); err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to create backfill index: %w", err))
	}
	defer f.dropBackfillIndexBestEffort(job.SchemaName, indexName)

	if err := f.setPhase(ctx, job, state.PhaseSyncing); err != nil {
		return err
	}

	if err := f.renameBackfillLoop(ctx, job); err != nil {
		return f.fail(ctx, job, err)
	}

	if err := f.setPhase(ctx, job, state.PhaseValidating); err != nil {
		return err
	}

	remaining, err := f.countRenameMismatches(ctx, job)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("validation query failed: %w", err))
	}
	if remaining > 0 {
		return f.fail(ctx, job, fmt.Errorf("backfill incomplete: %d row(s) still not synced to %s", remaining, job.NewColumnName))
	}

	return f.setPhase(ctx, job, state.PhaseCompleted)
}

// columnType reads the exact, fully-specified type of an existing column
// (including modifiers like varchar(50) or numeric(10,2)) via
// format_type() — more robust than information_schema.columns.data_type,
// which discards that precision/length information.
func (f *DDLFlow) columnType(ctx context.Context, schema, table, column string) (string, error) {
	var typ string
	query := `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3 AND NOT a.attisdropped
	`
	err := f.Pool.QueryRow(ctx, query, schema, table, column).Scan(&typ)
	if err != nil {
		return "", fmt.Errorf("column %q not found on %s.%s: %w", column, schema, table, err)
	}
	return typ, nil
}

// renameSyncNames deterministically derives the sync trigger function and
// trigger names from the job — NOT persisted as separate state.Job
// fields, since they can always be recomputed identically from job.ID at
// rollback time.
func renameSyncNames(job *state.Job) (fnName, trgName string) {
	shortID := job.ID
	if len(shortID) > 16 {
		shortID = shortID[:16]
	}
	return "pgam_rename_sync_fn_" + shortID, "pgam_rename_sync_trg_" + shortID
}

// createRenameSyncTrigger sets up the bidirectional dual-write sync
// described in executeRenameColumn's doc comment. TG_OP is checked
// because referencing OLD.* in an INSERT-context trigger call is invalid
// in PostgreSQL (there is no "old row" yet), so INSERT and UPDATE need
// distinct sync logic:
//   - INSERT: whichever of the two columns is non-NULL wins and is copied
//     to the other (handles both a legacy INSERT that only sets the old
//     column and a new INSERT that only sets the new one).
//   - UPDATE: IS DISTINCT FROM against OLD.* detects which column this
//     specific statement actually changed, and propagates that one to the
//     other — so a no-op UPDATE that doesn't touch either column doesn't
//     spuriously overwrite anything.
func (f *DDLFlow) createRenameSyncTrigger(ctx context.Context, job *state.Job) error {
	fnName, trgName := renameSyncNames(job)
	oldCol := quoteIdent(job.ColumnName)
	newCol := quoteIdent(job.NewColumnName)

	// CREATE OR REPLACE (not plain CREATE) is deliberate: if a job is
	// retried after a partial/interrupted attempt (e.g. it crashed after
	// creating the function but before the trigger), a plain CREATE
	// FUNCTION would fail with "already exists" on retry — exactly the
	// kind of transient failure this whole tool exists to be resilient
	// against. Using OR REPLACE makes this step safely re-runnable,
	// consistent with the "IF NOT EXISTS" idempotency pattern used
	// throughout the rest of this package (ADD COLUMN IF NOT EXISTS,
	// CREATE INDEX CONCURRENTLY IF NOT EXISTS, etc.).
	createFnDDL := fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $pgam$
		BEGIN
			IF TG_OP = 'INSERT' THEN
				IF NEW.%s IS NULL AND NEW.%s IS NOT NULL THEN
					NEW.%s := NEW.%s;
				ELSIF NEW.%s IS NULL AND NEW.%s IS NOT NULL THEN
					NEW.%s := NEW.%s;
				END IF;
			ELSE
				IF NEW.%s IS DISTINCT FROM OLD.%s THEN
					NEW.%s := NEW.%s;
				ELSIF NEW.%s IS DISTINCT FROM OLD.%s THEN
					NEW.%s := NEW.%s;
				END IF;
			END IF;
			RETURN NEW;
		END;
		$pgam$ LANGUAGE plpgsql
	`, quoteIdent(fnName),
		newCol, oldCol, newCol, oldCol,
		oldCol, newCol, oldCol, newCol,
		newCol, newCol, oldCol, newCol,
		oldCol, oldCol, newCol, oldCol,
	)
	if _, err := f.Pool.Exec(ctx, createFnDDL); err != nil {
		return fmt.Errorf("failed to create sync trigger function: %w", err)
	}

	// PostgreSQL has no "CREATE TRIGGER IF NOT EXISTS" (unlike CREATE
	// TABLE/INDEX), so a defensive DROP TRIGGER IF EXISTS first is what
	// makes this step retry-safe, for the same reason OR REPLACE does
	// above — a leftover trigger from a previous partial attempt must not
	// turn a legitimate retry into an "already exists" failure.
	dropTrgDDL := fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", quoteIdent(trgName), qualifiedTable(job))
	if _, err := f.Pool.Exec(ctx, dropTrgDDL); err != nil {
		return fmt.Errorf("failed to drop any leftover sync trigger before recreating: %w", err)
	}

	createTrgDDL := fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s()",
		quoteIdent(trgName), qualifiedTable(job), quoteIdent(fnName))
	if _, err := f.Pool.Exec(ctx, createTrgDDL); err != nil {
		return fmt.Errorf("failed to create sync trigger: %w", err)
	}
	return nil
}

// renameBackfillLoop mirrors backfillLoop/runBackfillBatch (ADD_COLUMN's
// volatile-default path) but syncs job.NewColumnName from job.ColumnName
// instead of applying a fixed default expression.
func (f *DDLFlow) renameBackfillLoop(ctx context.Context, job *state.Job) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if f.Watcher != nil {
			if waiting, err := f.Watcher.CheckLockWait(ctx); err == nil && waiting {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(lockWaitBackoff):
				}
				continue
			}
		}

		rowsAffected, err := f.runRenameBackfillBatch(ctx, job)
		if err != nil {
			return fmt.Errorf("backfill batch failed: %w", err)
		}
		if rowsAffected == 0 {
			return nil
		}
		if err := f.Store.IncrementRowsProcessed(ctx, job.ID, rowsAffected); err != nil {
			log.Printf("ddlflow: failed to persist rows-processed counter for job %s: %v", job.ID, err)
		}
		// Keep the in-memory job in sync with what was just persisted —
		// the Store call above only updates the DATABASE row; without
		// this, the *state.Job the caller holds (and immediately renders
		// via progress.Compute right after Execute returns, e.g. the
		// CLI's post-migrate output) would show RowsProcessed=0 even
		// though the migration fully completed. A real bug found via
		// manual testing: the API/frontend never showed this (every
		// request re-fetches fresh from the store), but the CLI's
		// synchronous, immediate render after `migrate` did.
		job.RowsProcessed += rowsAffected
	}
}

// runRenameBackfillBatch syncs up to batchSize() rows where the new
// column is still NULL but the old one already has a value — a plain
// NULL-in-both row needs no backfill, it's already "in sync". Relies on
// the temporary partial index executeRenameColumn creates (see
// createBackfillIndex's doc comment) to keep this a fast Index Scan
// regardless of how far the backfill has progressed — no cursor/progress
// tracking needed here.
func (f *DDLFlow) runRenameBackfillBatch(ctx context.Context, job *state.Job) (int64, error) {
	batchSQL := fmt.Sprintf(`
		UPDATE %s SET %s = %s
		WHERE ctid = ANY(ARRAY(
			SELECT ctid FROM %s WHERE %s IS NULL AND %s IS NOT NULL LIMIT %d
		))
	`,
		qualifiedTable(job), quoteIdent(job.NewColumnName), quoteIdent(job.ColumnName),
		qualifiedTable(job), quoteIdent(job.NewColumnName), quoteIdent(job.ColumnName), f.batchSize(),
	)
	tag, err := f.Pool.Exec(ctx, batchSQL)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (f *DDLFlow) countRenameMismatches(ctx context.Context, job *state.Job) (int64, error) {
	var remaining int64
	query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s IS NULL AND %s IS NOT NULL`,
		qualifiedTable(job), quoteIdent(job.NewColumnName), quoteIdent(job.ColumnName))
	err := f.Pool.QueryRow(ctx, query).Scan(&remaining)
	return remaining, err
}

// backfillLoop runs UPDATE in batches until RowsAffected() returns 0.
// Before each batch, if a Watcher is set, CheckLockWait provides FR-06 awareness.
func (f *DDLFlow) backfillLoop(ctx context.Context, job *state.Job) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if f.Watcher != nil {
			if waiting, err := f.Watcher.CheckLockWait(ctx); err == nil && waiting {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(lockWaitBackoff):
				}
				continue
			}
		}

		rowsAffected, err := f.runBackfillBatch(ctx, job)
		if err != nil {
			return fmt.Errorf("backfill batch failed: %w", err)
		}
		if rowsAffected == 0 {
			return nil
		}
		// Best-effort: a failure to persist the running counter shouldn't
		// abort an otherwise-successful backfill batch — it's a display
		// metric (see Job.RowsProcessed's doc comment), not correctness-
		// critical state.
		if err := f.Store.IncrementRowsProcessed(ctx, job.ID, rowsAffected); err != nil {
			log.Printf("ddlflow: failed to persist rows-processed counter for job %s: %v", job.ID, err)
		}
		// Keep the in-memory job in sync with what was just persisted —
		// see renameBackfillLoop's identical line for why this matters
		// (a real bug: without it, the CLI's immediate post-migrate
		// output showed RowsProcessed=0 for a fully-completed backfill).
		job.RowsProcessed += rowsAffected
	}
}

// runBackfillBatch runs a single batch UPDATE. Relies on the temporary
// partial index executeExpandBackfill creates (see createBackfillIndex's
// doc comment for the full history: this function went through TWO
// earlier approaches — no progress tracking, then a ctid cursor — both
// found by load testing to degrade badly at scale for different reasons)
// to keep "WHERE col IS NULL LIMIT N" a fast Index Scan regardless of how
// far the backfill has progressed. ctid is used for the actual UPDATE's
// row targeting (not for progress tracking, just to identify exactly
// which physical rows the index scan found) so this works regardless of
// the PRIMARY KEY's type/presence.
func (f *DDLFlow) runBackfillBatch(ctx context.Context, job *state.Job) (int64, error) {
	batchSQL := fmt.Sprintf(`
		UPDATE %s SET %s = %s
		WHERE ctid = ANY(ARRAY(
			SELECT ctid FROM %s WHERE %s IS NULL LIMIT %d
		))
	`,
		qualifiedTable(job), quoteIdent(job.ColumnName), job.DefaultValue,
		qualifiedTable(job), quoteIdent(job.ColumnName), f.batchSize(),
	)

	tag, err := f.Pool.Exec(ctx, batchSQL)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (f *DDLFlow) countRemainingNulls(ctx context.Context, job *state.Job) (int64, error) {
	var remaining int64
	query := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s IS NULL", qualifiedTable(job), quoteIdent(job.ColumnName))
	err := f.Pool.QueryRow(ctx, query).Scan(&remaining)
	return remaining, err
}

// Rollback reverses whichever flow the job used:
//   - ADD_COLUMN / Direct DDL / Expand&Backfill: drops the added column.
//     Since the original table is never touched, no data is lost for an
//     in-progress or failed job (FR-07).
//   - DROP_COLUMN: renames the column back from its deprecated (soft-drop)
//     name to its original name — see executeDropColumn. Only valid while
//     job.Phase == ROLLBACK_WINDOW and the deadline hasn't passed; once
//     internal/reaper's finalizeDropColumn has actually dropped the
//     column, there is nothing left to roll back.
//
// SAFETY GUARD (ADD_COLUMN path only): unlike internal/shadowflow (which
// has an explicit, time-boxed ROLLBACK_WINDOW per FR-08a), a COMPLETED
// ADD_COLUMN job has no such window — the added column may already be
// read or written by the application. Rollback therefore REFUSES to act
// on a COMPLETED ADD_COLUMN job: undoing it automatically could silently
// drop live, in-use data. Reverting a completed ADD_COLUMN migration must
// be a new, deliberate migration (e.g. an explicit DROP COLUMN request),
// not an accidental "rollback" of something the application may now
// depend on. DROP_COLUMN jobs don't need this guard: they already go
// through their OWN explicit rollback window before anything is deleted.
// Rollback reverses whichever operation the job ran — see the doc comments
// on Execute's sub-functions for the reasoning behind each operation's
// specific rollback semantics (some need a time-boxed window, some don't,
// depending on whether the operation is destructive to data).
func (f *DDLFlow) Rollback(ctx context.Context, job *state.Job) error {
	switch job.Operation {
	case "DROP_COLUMN":
		return f.rollbackDropColumn(ctx, job)
	case "ADD_INDEX":
		return f.rollbackAddIndex(ctx, job)
	case "DROP_INDEX":
		return f.rollbackDropIndex(ctx, job)
	case "SET_NOT_NULL":
		return f.rollbackSetNotNull(ctx, job)
	case "ADD_CONSTRAINT":
		return f.rollbackAddConstraint(ctx, job)
	case "RENAME_COLUMN":
		return f.rollbackRenameColumn(ctx, job)
	default: // ADD_COLUMN (also the fallback for older jobs with no Operation recorded)
		return f.rollbackAddColumn(ctx, job)
	}
}

// rollbackAddColumn reverses ADD_COLUMN (Direct DDL or Expand&Backfill) by
// dropping the added column. Since the original table is never touched,
// no data is lost for an in-progress or failed job (FR-07).
//
// SAFETY GUARD: unlike internal/shadowflow (which has an explicit,
// time-boxed ROLLBACK_WINDOW per FR-08a), a COMPLETED ADD_COLUMN job has
// no such window — the added column may already be read or written by the
// application. Rollback therefore REFUSES to act on a COMPLETED job:
// undoing it automatically could silently drop live, in-use data.
// Reverting a completed ADD_COLUMN migration must be a new, deliberate
// migration (e.g. an explicit DROP COLUMN request), not an accidental
// "rollback" of something the application may now depend on.
func (f *DDLFlow) rollbackAddColumn(ctx context.Context, job *state.Job) error {
	if job.Phase == state.PhaseCompleted {
		return fmt.Errorf("ddlflow: refusing to roll back a COMPLETED migration — " +
			"the added column may already be in use by the application; " +
			"this must be handled as a new, explicit migration (e.g. DROP COLUMN), not a rollback")
	}

	ddl := fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", qualifiedTable(job), quoteIdent(job.ColumnName))
	if err := execDDLWithLockTimeout(ctx, f.Pool, ddl); err != nil {
		return fmt.Errorf("rollback (DROP COLUMN) failed: %w", err)
	}
	return f.setPhase(ctx, job, state.PhaseAborted)
}

// rollbackDropColumn reverses a DROP_COLUMN soft-drop by renaming the
// deprecated column back to its original name.
func (f *DDLFlow) rollbackDropColumn(ctx context.Context, job *state.Job) error {
	if job.Phase != state.PhaseRollbackWindow {
		return fmt.Errorf("ddlflow: cannot roll back DROP_COLUMN job in phase %s (expected ROLLBACK_WINDOW) — "+
			"either the column was never soft-dropped, or the window already closed and internal/reaper "+
			"has already finalized (permanently dropped) it", job.Phase)
	}
	if job.RollbackDeadline != nil && time.Now().UTC().After(*job.RollbackDeadline) {
		return fmt.Errorf("ddlflow: the rollback window for this DROP_COLUMN expired at %s; "+
			"the column has likely already been finalized (permanently dropped) by internal/reaper",
			job.RollbackDeadline.Format(time.RFC3339))
	}
	if job.DeprecatedColumnName == "" {
		return fmt.Errorf("ddlflow: job has no recorded deprecated column name — cannot determine what to rename back")
	}

	ddl := fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
		qualifiedTable(job), quoteIdent(job.DeprecatedColumnName), quoteIdent(job.ColumnName))
	if err := execDDLWithLockTimeout(ctx, f.Pool, ddl); err != nil {
		return fmt.Errorf("rollback (restore column name) failed: %w", err)
	}
	return f.setPhase(ctx, job, state.PhaseAborted)
}

// rollbackAddIndex reverses ADD_INDEX by dropping the index that was
// created. Unlike rollbackAddColumn, this carries no "refuse if
// COMPLETED" guard: an index's existence is never something application
// CORRECTNESS depends on (only performance), so it's always safe to drop,
// at any time.
func (f *DDLFlow) rollbackAddIndex(ctx context.Context, job *state.Job) error {
	if job.IndexName == "" {
		return fmt.Errorf("ddlflow: no recorded index name on this job — cannot determine what to drop")
	}
	if err := f.dropIndexConcurrently(ctx, job.SchemaName, job.IndexName); err != nil {
		return fmt.Errorf("rollback (DROP INDEX) failed: %w", err)
	}
	return f.setPhase(ctx, job, state.PhaseAborted)
}

// rollbackDropIndex reverses DROP_INDEX by recreating the index from its
// pg_get_indexdef() definition captured just before the drop (see
// executeDropIndex). Deliberately has NO phase/deadline restriction —
// recreating an index is purely additive and safe at any time, unlike
// rollbackDropColumn's ROLLBACK_WINDOW requirement.
func (f *DDLFlow) rollbackDropIndex(ctx context.Context, job *state.Job) error {
	if job.IndexDefinition == "" {
		return fmt.Errorf("ddlflow: no captured index definition on this job — cannot recreate")
	}
	ddl := makeConcurrent(job.IndexDefinition)
	if _, err := f.Pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("rollback (recreate index) failed: %w", err)
	}
	return f.setPhase(ctx, job, state.PhaseAborted)
}

// rollbackSetNotNull reverses SET_NOT_NULL. If the job never made it past
// VALIDATING, NOT NULL was never actually applied — only the temporary
// CHECK constraint needs cleaning up. If it reached COMPLETED, NOT NULL is
// live; removing it is always safe at any time (loosens a rule for FUTURE
// writes only — no existing row is touched or at risk), so no time-boxed
// window is needed the way DROP_COLUMN requires one.
func (f *DDLFlow) rollbackSetNotNull(ctx context.Context, job *state.Job) error {
	switch job.Phase {
	case state.PhasePreparation, state.PhaseValidating:
		if job.ConstraintName != "" {
			ddl := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", qualifiedTable(job), quoteIdent(job.ConstraintName))
			if err := execDDLWithLockTimeout(ctx, f.Pool, ddl); err != nil {
				return fmt.Errorf("rollback (drop check constraint) failed: %w", err)
			}
		}
	case state.PhaseCompleted:
		ddl := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL", qualifiedTable(job), quoteIdent(job.ColumnName))
		if err := execDDLWithLockTimeout(ctx, f.Pool, ddl); err != nil {
			return fmt.Errorf("rollback (DROP NOT NULL) failed: %w", err)
		}
	default:
		return fmt.Errorf("ddlflow: cannot roll back SET_NOT_NULL job in phase %s", job.Phase)
	}
	return f.setPhase(ctx, job, state.PhaseAborted)
}

// rollbackAddConstraint reverses ADD_CONSTRAINT by dropping the
// constraint — safe at any time for the same reason rollbackSetNotNull's
// COMPLETED case is: removing a CHECK constraint only loosens future
// writes, it never touches or endangers existing data.
func (f *DDLFlow) rollbackAddConstraint(ctx context.Context, job *state.Job) error {
	if job.ConstraintName == "" {
		return fmt.Errorf("ddlflow: no recorded constraint name on this job — cannot determine what to drop")
	}
	ddl := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", qualifiedTable(job), quoteIdent(job.ConstraintName))
	if err := execDDLWithLockTimeout(ctx, f.Pool, ddl); err != nil {
		return fmt.Errorf("rollback (DROP CONSTRAINT) failed: %w", err)
	}
	return f.setPhase(ctx, job, state.PhaseAborted)
}

// rollbackRenameColumn reverses RENAME_COLUMN by dropping the sync
// trigger, its function, and the new column — leaving the original (old
// name) column completely untouched. Safe at ANY time, even long after
// COMPLETED: the "dual-write" state this operation reaches is additive
// infrastructure only (a new column plus a trigger), so removing it never
// endangers the original data, unlike DROP_COLUMN's genuinely destructive
// finalization step.
func (f *DDLFlow) rollbackRenameColumn(ctx context.Context, job *state.Job) error {
	fnName, trgName := renameSyncNames(job)

	dropTrgDDL := fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", quoteIdent(trgName), qualifiedTable(job))
	if _, err := f.Pool.Exec(ctx, dropTrgDDL); err != nil {
		return fmt.Errorf("rollback (drop sync trigger) failed: %w", err)
	}

	dropFnDDL := fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", quoteIdent(fnName))
	if _, err := f.Pool.Exec(ctx, dropFnDDL); err != nil {
		return fmt.Errorf("rollback (drop sync function) failed: %w", err)
	}

	if job.NewColumnName != "" {
		dropColDDL := fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", qualifiedTable(job), quoteIdent(job.NewColumnName))
		if _, err := f.Pool.Exec(ctx, dropColDDL); err != nil {
			return fmt.Errorf("rollback (drop new column) failed: %w", err)
		}
	}

	return f.setPhase(ctx, job, state.PhaseAborted)
}

func (f *DDLFlow) batchSize() int {
	if f.BatchSize <= 0 {
		return defaultBatchSize
	}
	return f.BatchSize
}

func (f *DDLFlow) setPhase(ctx context.Context, job *state.Job, phase state.Phase) error {
	if err := f.Store.UpdatePhase(ctx, job.ID, phase); err != nil {
		return fmt.Errorf("failed to update phase (%s): %w", phase, err)
	}
	job.Phase = phase
	return nil
}

// fail marks the job as FAILED, writes the original error to LastError, and
// returns the original error unchanged to the caller.
func (f *DDLFlow) fail(ctx context.Context, job *state.Job, cause error) error {
	if err := f.Store.UpdatePhaseWithError(ctx, job.ID, state.PhaseFailed, cause.Error()); err != nil {
		// If the checkpoint update also fails, combine both errors.
		return fmt.Errorf("%w (also failed to update checkpoint: %v)", cause, err)
	}
	job.Phase = state.PhaseFailed
	job.LastError = cause.Error()
	return cause
}

// ddlLockTimeout/ddlLockTimeoutMaxAttempts tune execDDLWithLockTimeout —
// see its doc comment for the full reasoning. 3 seconds is short enough
// to fail fast and free up the lock queue quickly, long enough to
// accommodate a typical short-lived transaction finishing naturally; 5
// attempts with escalating backoff (0.5s, 1s, 1.5s, 2s, 2.5s ≈ 7.5s
// worst case) gives a slow-but-finite transaction a real chance to clear
// without turning a stuck migration into an indefinite hang.
const (
	ddlLockTimeout            = 3 * time.Second
	ddlLockTimeoutMaxAttempts = 5
)

// execDDLWithLockTimeout runs a single DDL statement with a short
// lock_timeout, retrying with backoff if it can't acquire the lock in
// time, instead of a plain, unbounded pool.Exec.
//
// Why this exists — found via a real 10M-row load test, not a
// theoretical concern: even a PostgreSQL 11+ "fast path" ALTER TABLE
// (e.g. ADD COLUMN with a constant default, genuinely a metadata-only,
// near-instant change) still needs a brief ACCESS EXCLUSIVE lock to make
// that change. If the table has any concurrent activity at all, that
// ALTER TABLE may need to wait in the lock queue for currently-running
// transactions to finish first. The real danger isn't that wait itself —
// it's that PostgreSQL's lock queue is FIFO and deliberately
// non-starving: once ALTER TABLE is queued waiting for ACCESS EXCLUSIVE,
// every subsequent lock request on that table — including a plain SELECT
// that would normally be perfectly compatible with other readers — queues
// up BEHIND it too, to guarantee the DDL isn't starved forever by a
// constant stream of new readers. The load test measured exactly this:
// p99 query latency jumped from 11ms to 1.09s during an ADD_COLUMN that,
// on its own, is essentially instant — the 1+ second wasn't the DDL
// itself, it was every other query queued up waiting behind it.
//
// A short lock_timeout turns "the DDL statement quietly waits, and
// silently queues up all other traffic behind it" into "the DDL
// statement fails fast (SQLSTATE 55P03) if it can't get in quickly, and
// this function retries a few times" — the same conflicting transaction
// usually finishes within a couple of the short retry windows, and even
// in the worst case, ordinary application traffic is never blocked
// behind a DDL statement stuck waiting indefinitely.
func execDDLWithLockTimeout(ctx context.Context, pool *pgxpool.Pool, ddl string) error {
	var lastErr error
	for attempt := 1; attempt <= ddlLockTimeoutMaxAttempts; attempt++ {
		err := execOnceWithLockTimeout(ctx, pool, ddl)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isLockTimeoutError(err) {
			return err // a real failure (syntax, permissions, constraint violation, ...) — surface immediately, don't retry
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

// execOnceWithLockTimeout runs SET LOCAL lock_timeout and the DDL
// statement together in a single explicit transaction — SET LOCAL
// (unlike a plain SET) only affects the current transaction, automatically
// reverting at COMMIT/ROLLBACK, so this can never "leak" a tightened
// lock_timeout onto some unrelated later query that happens to reuse the
// same pooled connection.
func execOnceWithLockTimeout(ctx context.Context, pool *pgxpool.Pool, ddl string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit has succeeded

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%s'", ddlLockTimeout)); err != nil {
		return fmt.Errorf("failed to set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isLockTimeoutError reports whether err is PostgreSQL's SQLSTATE 55P03
// ("lock_not_available") — raised specifically when a statement's
// lock_timeout expires before it could acquire the lock it needed.
func isLockTimeoutError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return false
}

func qualifiedTable(job *state.Job) string {
	return quoteIdent(job.SchemaName) + "." + quoteIdent(job.TableName)
}

// quoteIdent applies simple escaping for identifiers in DDL statements
// (where parameters cannot be bound). Same logic as
// internal/reaper.quoteIdent; moving both to a shared internal/dbutil
// package is recommended later (TODO).
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

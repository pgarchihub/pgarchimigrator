//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/ddlflow/... -tags=integration -v
package ddlflow_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/ddlflow"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

const logicalDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable"

func connectPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, logicalDSN)
	if err != nil {
		t.Fatalf("could not connect (is docker compose up?): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestStore(t *testing.T) state.Store {
	t.Helper()
	dbPath := t.TempDir() + "/ddlflow-test.db"
	store, err := state.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("could not create state store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// createTestTable creates an isolated, uniquely-named table for each test.
// renameSyncFnName mirrors ddlflow's unexported renameSyncNames naming
// scheme (duplicated here since these are external package tests) — used
// only to clean up the sync trigger function RENAME_COLUMN tests create,
// so repeated test runs don't accumulate leftover schema objects (the
// trigger itself is cascaded away automatically when its table is
// dropped, but the function is a standalone schema object that isn't).
func renameSyncFnName(jobID string) string {
	shortID := jobID
	if len(shortID) > 16 {
		shortID = shortID[:16]
	}
	return "pgam_rename_sync_fn_" + shortID
}

func createTestTable(t *testing.T, pool *pgxpool.Pool, name string, rowCount int) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
		t.Fatalf("could not clean up old table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			existing_col TEXT NOT NULL
		)
	`, name)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if rowCount > 0 {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (existing_col)
			SELECT 'val' || g FROM generate_series(1, %d) g
		`, name, rowCount)); err != nil {
			t.Fatalf("could not insert test data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name))
	})
}

// TestExecute_DirectAddColumn_FixedDefault verifies the Section 4.0 row 1
// scenario: ADD COLUMN with a fixed default should complete with a single DDL.
func TestExecute_DirectAddColumn_FixedDefault(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_direct_test"
	createTestTable(t, pool, tableName, 100)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:           "direct-job-1",
		SchemaName:   "public",
		TableName:    tableName,
		Strategy:     "DIRECT_DDL",
		Phase:        state.PhasePreflight,
		Operation:    "ADD_COLUMN",
		ColumnName:   "status",
		ColumnType:   "TEXT",
		DefaultValue: "'active'",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	// Verify the column was really added and the default was applied to all rows.
	var count int
	err := pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE status = 'active'`, tableName),
	).Scan(&count)
	if err != nil {
		t.Fatalf("validation query failed: %v", err)
	}
	if count != 100 {
		t.Errorf("expected 100 rows with status='active', got %d", count)
	}

	// Verify the checkpoint also correctly records COMPLETED.
	got, err := store.Get(context.Background(), "direct-job-1")
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseCompleted {
		t.Errorf("expected checkpoint Phase=COMPLETED, got %s", got.Phase)
	}
}

// TestExecute_ExpandBackfill_VolatileDefault verifies the Section 4.0 row 2
// scenario: a volatile default should be added as NULL and backfilled in
// the background. Uses a small BatchSize to also verify multiple batch rounds.
func TestExecute_ExpandBackfill_VolatileDefault(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_backfill_test"
	createTestTable(t, pool, tableName, 250) // requires 3 rounds with BatchSize=100

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)
	flow.BatchSize = 100

	job := &state.Job{
		ID:                "backfill-job-1",
		SchemaName:        "public",
		TableName:         tableName,
		Strategy:          "EXPAND_BACKFILL",
		Phase:             state.PhasePreflight,
		Operation:         "ADD_COLUMN",
		ColumnName:        "created_ts",
		ColumnType:        "TIMESTAMPTZ",
		DefaultValue:      "now()",
		IsVolatileDefault: true,
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	// Verify no row is left NULL.
	var remaining int
	err := pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE created_ts IS NULL`, tableName),
	).Scan(&remaining)
	if err != nil {
		t.Fatalf("validation query failed: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 NULL rows, got %d", remaining)
	}

	// The running rows-processed counter (see Job.RowsProcessed's doc
	// comment) should end up matching the actual row count, persisted
	// across all 3 batch rounds (BatchSize=100, 250 rows) via
	// Store.IncrementRowsProcessed after each one.
	if job.RowsProcessed != 250 {
		t.Errorf("expected RowsProcessed=250 after backfilling all rows, got %d", job.RowsProcessed)
	}
	persisted, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("could not read back job: %v", err)
	}
	if persisted.RowsProcessed != 250 {
		t.Errorf("expected the persisted RowsProcessed=250, got %d", persisted.RowsProcessed)
	}

	var total int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, tableName)).Scan(&total); err != nil {
		t.Fatalf("could not get total row count: %v", err)
	}
	if total != 250 {
		t.Errorf("total row count should have been preserved (250), got %d", total)
	}
}

// TestExecute_ExpandBackfill_TemporaryIndexCleanedUpOnSuccess verifies
// createBackfillIndex's whole other half — the temporary partial index
// isn't just fast, it also has to not be left behind after a successful
// migration (see executeExpandBackfill's `defer
// dropBackfillIndexBestEffort`). Checks for ANY index matching
// ddlflow.BackfillIndexPrefix on the table, not a specific name, since
// the exact name is a private implementation detail (backfillIndexName
// is unexported) — this test only needs to know none exist afterward,
// not what they would have been called.
func TestExecute_ExpandBackfill_TemporaryIndexCleanedUpOnSuccess(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_backfill_index_cleanup_test"
	createTestTable(t, pool, tableName, 50)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "backfill-index-cleanup-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "ADD_COLUMN", ColumnName: "created_ts", ColumnType: "TIMESTAMPTZ",
		DefaultValue: "now()", IsVolatileDefault: true,
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Fatalf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	var leftoverIndexes int
	query := `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'public' AND tablename = $1 AND indexname LIKE $2
	`
	if err := pool.QueryRow(context.Background(), query, tableName, ddlflow.BackfillIndexPrefix+"%").Scan(&leftoverIndexes); err != nil {
		t.Fatalf("could not check for leftover indexes: %v", err)
	}
	if leftoverIndexes != 0 {
		t.Errorf("expected the temporary backfill index to be dropped after a successful migration, found %d matching pg_indexes.%s%%",
			leftoverIndexes, ddlflow.BackfillIndexPrefix)
	}
}

// TestExecute_ExpandBackfill_ManyBatches_NoRowsSkipped is a direct
// regression guard for a real, load-test-found bug — and the fix went
// through TWO iterations, both caught by exactly this kind of
// many-rounds test:
//
//  1. The original runBackfillBatch re-ran "WHERE col IS NULL LIMIT N"
//     from scratch every batch, with no tracking of how far a PRIOR
//     batch had already scanned — an O(n²) performance cliff, and each
//     increasingly-slow batch held row-level locks for its whole
//     duration, blocking concurrent traffic for up to 33 seconds.
//  2. A ctid-based cursor was tried next, but EXPLAIN ANALYZE against a
//     real PostgreSQL 16 instance showed the planner didn't give it the
//     efficient TID Range Scan the fix assumed — it fell back to a
//     Parallel Seq Scan anyway, measuring stalls up to 63 seconds,
//     WORSE than the original.
//
// The actual fix (see createBackfillIndex's doc comment) is a temporary
// partial index on "WHERE col IS NULL" — no cursor, no progress tracking
// needed, since the index itself always makes the plain query fast. This
// test uses many more rounds (1,050 rows ÷ 100 per batch = 11 rounds,
// the last a partial batch of 50) specifically to make a correctness bug
// (skipping rows, or getting stuck re-processing the same rows and never
// terminating) much more likely to surface than a small 2-3-round test
// would — this stayed a valuable, valid test through both iterations of
// the fix above, and remains one for whatever comes next.
func TestExecute_ExpandBackfill_ManyBatches_NoRowsSkipped(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_backfill_many_batches_test"
	const totalRows = 1050
	const batchSize = 100 // 11 rounds: 10 full batches of 100 + 1 partial batch of 50
	createTestTable(t, pool, tableName, totalRows)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)
	flow.BatchSize = batchSize

	job := &state.Job{
		ID: "backfill-many-batches-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "ADD_COLUMN", ColumnName: "created_ts", ColumnType: "TIMESTAMPTZ",
		DefaultValue: "now()", IsVolatileDefault: true,
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	var remaining int
	if err := pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE created_ts IS NULL`, tableName),
	).Scan(&remaining); err != nil {
		t.Fatalf("validation query failed: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 NULL rows after %d rounds, got %d — some rows were skipped", (totalRows+batchSize-1)/batchSize, remaining)
	}

	// The strongest possible correctness check for a cursor bug: distinct
	// row IDENTITY count, not just a NULL/total count, since a bug that
	// re-updates the SAME rows repeatedly (getting stuck, never advancing
	// the cursor) could otherwise still coincidentally end with
	// remaining==0 while having skipped OTHER rows entirely — this
	// explicitly confirms every one of the 1,050 distinct ids was
	// actually touched exactly once's worth of "no longer NULL".
	var distinctBackfilled int
	if err := pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(DISTINCT id) FROM %s WHERE created_ts IS NOT NULL`, tableName),
	).Scan(&distinctBackfilled); err != nil {
		t.Fatalf("distinct-id validation query failed: %v", err)
	}
	if distinctBackfilled != totalRows {
		t.Errorf("expected all %d distinct rows to be backfilled, got %d distinct rows touched", totalRows, distinctBackfilled)
	}

	if job.RowsProcessed != totalRows {
		t.Errorf("expected RowsProcessed=%d, got %d", totalRows, job.RowsProcessed)
	}
}

// TestRollback_RefusesOnCompletedJob verifies the safety guard added after
// a real incident: DDLFlow has no rollback window (unlike shadowflow's
// FR-08a), so once a job is COMPLETED the added column may already be in
// use — Rollback must refuse rather than silently drop live data.
func TestRollback_RefusesOnCompletedJob(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_rollback_completed_test"
	createTestTable(t, pool, tableName, 10)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:           "rollback-completed-job-1",
		SchemaName:   "public",
		TableName:    tableName,
		Strategy:     "DIRECT_DDL",
		Phase:        state.PhasePreflight,
		Operation:    "ADD_COLUMN",
		ColumnName:   "temp_col",
		ColumnType:   "INTEGER",
		DefaultValue: "0",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Fatalf("expected the job to be COMPLETED after Execute, got %s", job.Phase)
	}

	err := flow.Rollback(context.Background(), job)
	if err == nil {
		t.Fatal("expected Rollback to refuse a COMPLETED job")
	}

	var colExists bool
	if scanErr := pool.QueryRow(context.Background(), `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_name = $1 AND column_name = 'temp_col'
		)
	`, tableName).Scan(&colExists); scanErr != nil {
		t.Fatalf("column check failed: %v", scanErr)
	}
	if !colExists {
		t.Error("temp_col should still exist — Rollback must not have touched it")
	}
}

// TestRollback_NonCompletedJob_RemovesAddedColumn verifies FR-07 for the
// case Rollback is actually meant to handle: an in-progress or failed job
// (never reached COMPLETED). The original data must be untouched and the
// added column removed.
func TestRollback_NonCompletedJob_RemovesAddedColumn(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_rollback_inprogress_test"
	createTestTable(t, pool, tableName, 10)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:           "rollback-inprogress-job-1",
		SchemaName:   "public",
		TableName:    tableName,
		Strategy:     "DIRECT_DDL",
		Phase:        state.PhasePreflight,
		Operation:    "ADD_COLUMN",
		ColumnName:   "temp_col",
		ColumnType:   "INTEGER",
		DefaultValue: "0",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	// Simulate an interruption partway through: the column was added (as
	// executeDirectAddColumn's first and only DDL step would do), but the
	// job never reached COMPLETED — e.g. the process crashed right after.
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN temp_col INTEGER DEFAULT 0`, tableName,
	)); err != nil {
		t.Fatalf("could not simulate the partial column add: %v", err)
	}
	if err := store.UpdatePhase(context.Background(), job.ID, state.PhasePreparation); err != nil {
		t.Fatalf("could not set phase: %v", err)
	}
	job.Phase = state.PhasePreparation

	if err := flow.Rollback(context.Background(), job); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	var colExists bool
	err := pool.QueryRow(context.Background(), `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_name = $1 AND column_name = 'temp_col'
		)
	`, tableName).Scan(&colExists)
	if err != nil {
		t.Fatalf("column check failed: %v", err)
	}
	if colExists {
		t.Error("temp_col column still exists after rollback")
	}

	var rowCount int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, tableName)).Scan(&rowCount); err != nil {
		t.Fatalf("could not get row count: %v", err)
	}
	if rowCount != 10 {
		t.Errorf("original 10 rows should have been preserved, got %d", rowCount)
	}
}

// TestExecute_DropColumn_SoftDropEntersRollbackWindow verifies the
// two-phase DROP_COLUMN soft-drop: the original column name must become
// unreachable immediately (the intended forcing function — see
// executeDropColumn's doc comment), while the data survives intact under
// the deprecated name, and the job lands in ROLLBACK_WINDOW with a
// deadline rather than COMPLETED.
func TestExecute_DropColumn_SoftDropEntersRollbackWindow(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_dropcol_soft_test"
	createTestTable(t, pool, tableName, 5)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:         "dropcol-soft-job-1",
		SchemaName: "public",
		TableName:  tableName,
		Strategy:   "DIRECT_DDL",
		Phase:      state.PhasePreflight,
		Operation:  "DROP_COLUMN",
		ColumnName: "existing_col",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if job.Phase != state.PhaseRollbackWindow {
		t.Errorf("expected Phase=ROLLBACK_WINDOW, got %s", job.Phase)
	}
	if job.DeprecatedColumnName == "" {
		t.Fatal("expected a deprecated column name to be recorded")
	}
	if job.RollbackDeadline == nil {
		t.Fatal("expected a rollback deadline to be set")
	}

	// The original column name must no longer be reachable — this is the
	// intended forcing function (see executeDropColumn's doc comment).
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`SELECT existing_col FROM %s LIMIT 1`, tableName)); err == nil {
		t.Error("expected the original column name to no longer exist after soft-drop")
	}

	// But the data must still be fully intact under the deprecated name.
	var count int
	checkQuery := fmt.Sprintf(`SELECT count(*) FROM %s WHERE "%s" IS NOT NULL`, tableName, job.DeprecatedColumnName)
	if err := pool.QueryRow(context.Background(), checkQuery).Scan(&count); err != nil {
		t.Fatalf("could not query the deprecated column: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5 rows with non-null data under the deprecated column name, got %d", count)
	}

	// Also verify the checkpoint persisted everything correctly.
	got, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseRollbackWindow || got.DeprecatedColumnName != job.DeprecatedColumnName {
		t.Errorf("checkpoint did not persist the expected state: %+v", got)
	}
}

// TestRollback_DropColumn_WithinWindow_RestoresColumn verifies that
// calling Rollback while still inside the window renames the column back
// to its original name, with the data fully intact — the "I changed my
// mind" path the two-phase design exists for.
func TestRollback_DropColumn_WithinWindow_RestoresColumn(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_dropcol_rollback_test"
	createTestTable(t, pool, tableName, 7)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:         "dropcol-rollback-job-1",
		SchemaName: "public",
		TableName:  tableName,
		Strategy:   "DIRECT_DDL",
		Phase:      state.PhasePreflight,
		Operation:  "DROP_COLUMN",
		ColumnName: "existing_col",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if err := flow.Rollback(context.Background(), job); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED after rollback, got %s", job.Phase)
	}

	// The original column name must be queryable again, with the data intact.
	var count int
	checkQuery := fmt.Sprintf(`SELECT count(*) FROM %s WHERE existing_col IS NOT NULL`, tableName)
	if err := pool.QueryRow(context.Background(), checkQuery).Scan(&count); err != nil {
		t.Fatalf("expected the original column name to be queryable again after rollback: %v", err)
	}
	if count != 7 {
		t.Errorf("expected all 7 rows' data to survive the round trip, got %d", count)
	}
}

// TestRollback_DropColumn_ExpiredWindow_Refuses verifies Rollback refuses
// to act once the deadline has passed — matching internal/shadowflow's
// equivalent FR-08a guard. A tiny DropColumnRollbackWindow is used so the
// test doesn't need to sleep for the real (10 minute) default.
func TestRollback_DropColumn_ExpiredWindow_Refuses(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_dropcol_expired_test"
	createTestTable(t, pool, tableName, 3)

	store := newTestStore(t)
	flow := &ddlflow.DDLFlow{Pool: pool, Store: store, DropColumnRollbackWindow: 1 * time.Millisecond}

	job := &state.Job{
		ID:         "dropcol-expired-job-1",
		SchemaName: "public",
		TableName:  tableName,
		Strategy:   "DIRECT_DDL",
		Phase:      state.PhasePreflight,
		Operation:  "DROP_COLUMN",
		ColumnName: "existing_col",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	time.Sleep(10 * time.Millisecond) // let the 1ms window definitely elapse

	if err := flow.Rollback(context.Background(), job); err == nil {
		t.Error("expected Rollback to refuse once the rollback window has expired")
	}
}

// TestExecute_AddIndex_CreatesValidIndex verifies CREATE INDEX CONCURRENTLY
// actually produces a usable index and the job completes normally (no
// rollback window needed — see executeAddIndex's doc comment).
func TestExecute_AddIndex_CreatesValidIndex(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_addindex_test"
	createTestTable(t, pool, tableName, 20)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:         "addindex-job-1",
		SchemaName: "public",
		TableName:  tableName,
		Strategy:   "DIRECT_DDL",
		Phase:      state.PhasePreflight,
		Operation:  "ADD_INDEX",
		ColumnName: "existing_col",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}
	wantName := "idx_" + tableName + "_existing_col"
	if job.IndexName != wantName {
		t.Errorf("expected auto-generated index name %q, got %q", wantName, job.IndexName)
	}

	var valid bool
	checkQuery := `
		SELECT i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1
	`
	if err := pool.QueryRow(context.Background(), checkQuery, job.IndexName).Scan(&valid); err != nil {
		t.Fatalf("index validity check failed: %v", err)
	}
	if !valid {
		t.Error("expected the created index to be valid")
	}
}

// TestRollback_AddIndex_DropsTheIndex verifies ADD_INDEX's rollback works
// even AFTER the job reached COMPLETED — unlike ADD_COLUMN, an index
// carries no "the application might already depend on this" correctness
// risk, only a performance one, so no guard is needed.
func TestRollback_AddIndex_DropsTheIndex(t *testing.T) {
	pool := connectPool(t)
	tableName := "ddlflow_addindex_rollback_test"
	createTestTable(t, pool, tableName, 5)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:         "addindex-rollback-job-1",
		SchemaName: "public",
		TableName:  tableName,
		Strategy:   "DIRECT_DDL",
		Phase:      state.PhasePreflight,
		Operation:  "ADD_INDEX",
		ColumnName: "existing_col",
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(context.Background(), job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Fatalf("expected Phase=COMPLETED before testing rollback-after-completion, got %s", job.Phase)
	}

	if err := flow.Rollback(context.Background(), job); err != nil {
		t.Fatalf("Rollback failed (should succeed even after COMPLETED for ADD_INDEX): %v", err)
	}
	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED after rollback, got %s", job.Phase)
	}

	var exists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = $1)`
	if err := pool.QueryRow(context.Background(), checkQuery, job.IndexName).Scan(&exists); err != nil {
		t.Fatalf("index existence check failed: %v", err)
	}
	if exists {
		t.Error("expected the index to have been dropped by rollback")
	}
}

// TestExecute_DropIndex_CapturesDefinitionAndDrops verifies DROP INDEX
// CONCURRENTLY actually removes the index, and that the pre-drop
// definition was captured (needed for rollback — see the next test).
func TestExecute_DropIndex_CapturesDefinitionAndDrops(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_dropindex_test"
	createTestTable(t, pool, tableName, 5)

	indexName := "idx_dropindex_test_manual"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX %s ON %s (existing_col)`, indexName, tableName)); err != nil {
		t.Fatalf("could not create the index to be dropped: %v", err)
	}

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:         "dropindex-job-1",
		SchemaName: "public",
		TableName:  tableName,
		Strategy:   "DIRECT_DDL",
		Phase:      state.PhasePreflight,
		Operation:  "DROP_INDEX",
		IndexName:  indexName,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}
	if job.IndexDefinition == "" {
		t.Fatal("expected the pre-drop index definition to have been captured")
	}
	if !strings.Contains(job.IndexDefinition, "existing_col") {
		t.Errorf("expected the captured definition to reference existing_col, got: %s", job.IndexDefinition)
	}

	var exists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = $1)`
	if err := pool.QueryRow(ctx, checkQuery, indexName).Scan(&exists); err != nil {
		t.Fatalf("index existence check failed: %v", err)
	}
	if exists {
		t.Error("expected the index to have actually been dropped")
	}
}

// TestRollback_DropIndex_RecreatesIndex verifies DROP_INDEX's rollback
// recreates an equivalent index from the captured definition — and,
// unlike DROP_COLUMN, this must work with no time limit and no phase
// restriction (tested here well after COMPLETED, with no rollback window
// involved at all).
func TestRollback_DropIndex_RecreatesIndex(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_dropindex_rollback_test"
	createTestTable(t, pool, tableName, 5)

	indexName := "idx_dropindex_rollback_test_manual"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX %s ON %s (existing_col)`, indexName, tableName)); err != nil {
		t.Fatalf("could not create the index to be dropped: %v", err)
	}

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID:         "dropindex-rollback-job-1",
		SchemaName: "public",
		TableName:  tableName,
		Strategy:   "DIRECT_DDL",
		Phase:      state.PhasePreflight,
		Operation:  "DROP_INDEX",
		IndexName:  indexName,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if err := flow.Rollback(ctx, job); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED after rollback, got %s", job.Phase)
	}

	var exists, valid bool
	checkQuery := `
		SELECT true, i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1
	`
	err := pool.QueryRow(ctx, checkQuery, indexName).Scan(&exists, &valid)
	if err != nil {
		t.Fatalf("expected the index to have been recreated by rollback, but lookup failed: %v", err)
	}
	if !exists || !valid {
		t.Errorf("expected a valid recreated index, got exists=%v valid=%v", exists, valid)
	}
}

// TestExecute_SetNotNull_Succeeds verifies the "expand and validate"
// pattern actually enforces NOT NULL — the temporary check constraint is
// validated, SET NOT NULL succeeds, and the constraint is cleaned up
// afterward since it's redundant once the column flag itself enforces it.
func TestExecute_SetNotNull_Succeeds(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_setnotnull_test"
	createTestTable(t, pool, tableName, 5)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN nullable_col TEXT`, tableName)); err != nil {
		t.Fatalf("could not add nullable column: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET nullable_col = 'x'`, tableName)); err != nil {
		t.Fatalf("could not populate nullable column: %v", err)
	}

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "setnotnull-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "DIRECT_DDL", Phase: state.PhasePreflight,
		Operation: "SET_NOT_NULL", ColumnName: "nullable_col",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	var isNullable string
	checkQuery := `SELECT is_nullable FROM information_schema.columns WHERE table_name = $1 AND column_name = 'nullable_col'`
	if err := pool.QueryRow(ctx, checkQuery, tableName).Scan(&isNullable); err != nil {
		t.Fatalf("column check failed: %v", err)
	}
	if isNullable != "NO" {
		t.Errorf("expected nullable_col to be NOT NULL, got is_nullable=%s", isNullable)
	}

	var constraintExists bool
	constraintQuery := `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = $1)`
	if err := pool.QueryRow(ctx, constraintQuery, job.ConstraintName).Scan(&constraintExists); err != nil {
		t.Fatalf("constraint check failed: %v", err)
	}
	if constraintExists {
		t.Error("expected the temporary check constraint to have been dropped after SET NOT NULL succeeded")
	}
}

// TestExecute_SetNotNull_FailsOnExistingNull verifies validation catches
// an existing NULL and cleans up the broken constraint rather than
// leaving it behind (same defense-in-depth spirit as ADD_INDEX's
// invalid-index cleanup).
func TestExecute_SetNotNull_FailsOnExistingNull(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_setnotnull_fail_test"
	createTestTable(t, pool, tableName, 3)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN nullable_col TEXT`, tableName)); err != nil {
		t.Fatalf("could not add nullable column: %v", err)
	}
	// Deliberately leave nullable_col NULL for every row.

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "setnotnull-fail-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "DIRECT_DDL", Phase: state.PhasePreflight,
		Operation: "SET_NOT_NULL", ColumnName: "nullable_col",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(ctx, job); err == nil {
		t.Fatal("expected Execute to fail due to existing NULL values")
	}
	if job.Phase != state.PhaseFailed {
		t.Errorf("expected Phase=FAILED, got %s", job.Phase)
	}

	var constraintExists bool
	constraintQuery := `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = $1)`
	if err := pool.QueryRow(ctx, constraintQuery, job.ConstraintName).Scan(&constraintExists); err != nil {
		t.Fatalf("constraint check failed: %v", err)
	}
	if constraintExists {
		t.Error("expected the failed check constraint to have been cleaned up, not left behind")
	}
}

// TestRollback_SetNotNull_AfterCompleted_DropsNotNull verifies rollback
// works even long after COMPLETED — unlike DROP_COLUMN, no time-boxed
// window is needed since loosening NOT NULL never touches existing data.
func TestRollback_SetNotNull_AfterCompleted_DropsNotNull(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_setnotnull_rollback_test"
	createTestTable(t, pool, tableName, 3)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN nullable_col TEXT`, tableName)); err != nil {
		t.Fatalf("could not add nullable column: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET nullable_col = 'x'`, tableName)); err != nil {
		t.Fatalf("could not populate nullable column: %v", err)
	}

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "setnotnull-rollback-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "DIRECT_DDL", Phase: state.PhasePreflight,
		Operation: "SET_NOT_NULL", ColumnName: "nullable_col",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Fatalf("expected Phase=COMPLETED before testing rollback-after-completion, got %s", job.Phase)
	}

	if err := flow.Rollback(ctx, job); err != nil {
		t.Fatalf("Rollback failed (should succeed even after COMPLETED for SET_NOT_NULL): %v", err)
	}
	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED after rollback, got %s", job.Phase)
	}

	var isNullable string
	checkQuery := `SELECT is_nullable FROM information_schema.columns WHERE table_name = $1 AND column_name = 'nullable_col'`
	if err := pool.QueryRow(ctx, checkQuery, tableName).Scan(&isNullable); err != nil {
		t.Fatalf("column check failed: %v", err)
	}
	if isNullable != "YES" {
		t.Errorf("expected nullable_col to be nullable again after rollback, got is_nullable=%s", isNullable)
	}
}

// TestExecute_AddConstraint_Succeeds verifies a CHECK constraint is added
// and validated correctly when every existing row satisfies it.
func TestExecute_AddConstraint_Succeeds(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_addconstraint_test"
	createTestTable(t, pool, tableName, 5)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "addconstraint-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "DIRECT_DDL", Phase: state.PhasePreflight,
		Operation: "ADD_CONSTRAINT", ConstraintName: "existing_col_not_empty",
		CheckExpression: "length(existing_col) > 0",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	var valid bool
	checkQuery := `SELECT convalidated FROM pg_constraint WHERE conname = $1`
	if err := pool.QueryRow(ctx, checkQuery, job.ConstraintName).Scan(&valid); err != nil {
		t.Fatalf("constraint check failed: %v", err)
	}
	if !valid {
		t.Error("expected the constraint to be validated (convalidated=true)")
	}
}

// TestExecute_AddConstraint_FailsOnViolatingRow verifies validation
// catches an existing row that violates the check and cleans up rather
// than leaving a permanently-invalid constraint behind.
func TestExecute_AddConstraint_FailsOnViolatingRow(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_addconstraint_fail_test"
	createTestTable(t, pool, tableName, 0)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (existing_col) VALUES ('')`, tableName)); err != nil {
		t.Fatalf("could not seed a violating row: %v", err)
	}

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "addconstraint-fail-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "DIRECT_DDL", Phase: state.PhasePreflight,
		Operation: "ADD_CONSTRAINT", ConstraintName: "existing_col_not_empty_fail",
		CheckExpression: "length(existing_col) > 0",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(ctx, job); err == nil {
		t.Fatal("expected Execute to fail due to a violating row")
	}
	if job.Phase != state.PhaseFailed {
		t.Errorf("expected Phase=FAILED, got %s", job.Phase)
	}

	var constraintExists bool
	constraintQuery := `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = $1)`
	if err := pool.QueryRow(ctx, constraintQuery, job.ConstraintName).Scan(&constraintExists); err != nil {
		t.Fatalf("constraint check failed: %v", err)
	}
	if constraintExists {
		t.Error("expected the failed check constraint to have been cleaned up, not left behind")
	}
}

// TestRollback_AddConstraint_DropsConstraint verifies rollback works even
// well after COMPLETED, same reasoning as SET_NOT_NULL's rollback test.
func TestRollback_AddConstraint_DropsConstraint(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_addconstraint_rollback_test"
	createTestTable(t, pool, tableName, 5)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "addconstraint-rollback-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "DIRECT_DDL", Phase: state.PhasePreflight,
		Operation: "ADD_CONSTRAINT", ConstraintName: "existing_col_not_empty_rb",
		CheckExpression: "length(existing_col) > 0",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if err := flow.Rollback(ctx, job); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED after rollback, got %s", job.Phase)
	}

	var constraintExists bool
	constraintQuery := `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = $1)`
	if err := pool.QueryRow(ctx, constraintQuery, job.ConstraintName).Scan(&constraintExists); err != nil {
		t.Fatalf("constraint check failed: %v", err)
	}
	if constraintExists {
		t.Error("expected the constraint to have been dropped by rollback")
	}
}

// TestExecute_RenameColumn_BackfillsExistingRows verifies the new column
// is created and existing (pre-migration) rows are backfilled from the
// old column via the batched backfill loop.
func TestExecute_RenameColumn_BackfillsExistingRows(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_rename_test"
	createTestTable(t, pool, tableName, 5)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "rename-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "RENAME_COLUMN", ColumnName: "existing_col", NewColumnName: "renamed_col",
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s() CASCADE`, renameSyncFnName(job.ID)))
	})
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	var mismatched int
	checkQuery := fmt.Sprintf(`SELECT count(*) FROM %s WHERE renamed_col IS DISTINCT FROM existing_col`, tableName)
	if err := pool.QueryRow(ctx, checkQuery).Scan(&mismatched); err != nil {
		t.Fatalf("verification query failed: %v", err)
	}
	if mismatched != 0 {
		t.Errorf("expected all 5 pre-existing rows to be backfilled and matching, got %d mismatches", mismatched)
	}
}

// TestExecute_RenameColumn_ManyBatches_NoRowsSkipped is
// runRenameBackfillBatch's counterpart to
// TestExecute_ExpandBackfill_ManyBatches_NoRowsSkipped — see that test's
// doc comment for the full history (two prior fix attempts, both found
// flawed by load testing, before landing on the temporary partial index
// this now relies on). The existing
// TestExecute_RenameColumn_BackfillsExistingRows above only seeds 5 rows
// (a single batch at the default BatchSize), which would never actually
// exercise multiple rounds. This forces 11 rounds (1,050 rows ÷ 100 per
// batch) specifically to catch a bug a small single-batch test
// structurally cannot.
func TestExecute_RenameColumn_ManyBatches_NoRowsSkipped(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_rename_many_batches_test"
	const totalRows = 1050
	const batchSize = 100
	createTestTable(t, pool, tableName, totalRows)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)
	flow.BatchSize = batchSize

	job := &state.Job{
		ID: "rename-many-batches-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "RENAME_COLUMN", ColumnName: "existing_col", NewColumnName: "renamed_col",
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s() CASCADE`, renameSyncFnName(job.ID)))
	})
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED, got %s", job.Phase)
	}

	var mismatched int
	checkQuery := fmt.Sprintf(`SELECT count(*) FROM %s WHERE renamed_col IS DISTINCT FROM existing_col`, tableName)
	if err := pool.QueryRow(ctx, checkQuery).Scan(&mismatched); err != nil {
		t.Fatalf("verification query failed: %v", err)
	}
	if mismatched != 0 {
		t.Errorf("expected all %d rows to be backfilled and matching after %d rounds, got %d mismatches — some rows were skipped",
			totalRows, (totalRows+batchSize-1)/batchSize, mismatched)
	}

	if job.RowsProcessed != totalRows {
		t.Errorf("expected RowsProcessed=%d, got %d", totalRows, job.RowsProcessed)
	}
}

// TestExecute_RenameColumn_OldNameStillWorks is the core value
// proposition of this operation: legacy application code that only knows
// about the OLD column name must keep working seamlessly after the
// migration — a plain ALTER TABLE RENAME COLUMN would break this the
// instant it ran.
func TestExecute_RenameColumn_OldNameStillWorks(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_rename_oldname_test"
	createTestTable(t, pool, tableName, 0)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "rename-oldname-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "RENAME_COLUMN", ColumnName: "existing_col", NewColumnName: "renamed_col",
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s() CASCADE`, renameSyncFnName(job.ID)))
	})
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Simulate legacy application code: an INSERT that only sets the OLD
	// column name, exactly as it would have before this migration ever ran.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (existing_col) VALUES ('legacy-write')`, tableName)); err != nil {
		t.Fatalf("legacy-style INSERT (old column only) failed: %v", err)
	}

	var newValue string
	checkQuery := fmt.Sprintf(`SELECT renamed_col FROM %s WHERE existing_col = 'legacy-write'`, tableName)
	if err := pool.QueryRow(ctx, checkQuery).Scan(&newValue); err != nil {
		t.Fatalf("could not read back the synced new column: %v", err)
	}
	if newValue != "legacy-write" {
		t.Errorf("expected the sync trigger to propagate the legacy write to renamed_col, got %q", newValue)
	}
}

// TestExecute_RenameColumn_NewNameWorks verifies the symmetric case:
// application code redeployed to use the NEW column name also works, and
// keeps the old column in sync too (so a rollback, or any code still on
// the old name, sees consistent data).
func TestExecute_RenameColumn_NewNameWorks(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_rename_newname_test"
	createTestTable(t, pool, tableName, 0)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "rename-newname-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "RENAME_COLUMN", ColumnName: "existing_col", NewColumnName: "renamed_col",
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s() CASCADE`, renameSyncFnName(job.ID)))
	})
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Simulate NEW application code: an INSERT that only sets the NEW
	// column name.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (renamed_col) VALUES ('new-write')`, tableName)); err != nil {
		t.Fatalf("new-style INSERT (new column only) failed: %v", err)
	}

	var oldValue string
	checkQuery := fmt.Sprintf(`SELECT existing_col FROM %s WHERE renamed_col = 'new-write'`, tableName)
	if err := pool.QueryRow(ctx, checkQuery).Scan(&oldValue); err != nil {
		t.Fatalf("could not read back the synced old column: %v", err)
	}
	if oldValue != "new-write" {
		t.Errorf("expected the sync trigger to propagate the new write to existing_col, got %q", oldValue)
	}
}

// TestExecute_RenameColumn_UpdateSyncsBothDirections verifies the UPDATE
// path of the sync trigger (distinct logic from INSERT, since OLD.* is
// only meaningful for UPDATE) correctly propagates a change to whichever
// column a given statement actually touches.
func TestExecute_RenameColumn_UpdateSyncsBothDirections(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_rename_update_test"
	createTestTable(t, pool, tableName, 1)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "rename-update-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "RENAME_COLUMN", ColumnName: "existing_col", NewColumnName: "renamed_col",
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s() CASCADE`, renameSyncFnName(job.ID)))
	})
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Update via the OLD name — must propagate to the new column.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET existing_col = 'updated-via-old'`, tableName)); err != nil {
		t.Fatalf("UPDATE via old column failed: %v", err)
	}
	var viaOld string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT renamed_col FROM %s LIMIT 1`, tableName)).Scan(&viaOld); err != nil {
		t.Fatalf("could not read renamed_col: %v", err)
	}
	if viaOld != "updated-via-old" {
		t.Errorf("expected UPDATE via old column to sync to renamed_col, got %q", viaOld)
	}

	// Update via the NEW name — must propagate to the old column.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET renamed_col = 'updated-via-new'`, tableName)); err != nil {
		t.Fatalf("UPDATE via new column failed: %v", err)
	}
	var viaNew string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT existing_col FROM %s LIMIT 1`, tableName)).Scan(&viaNew); err != nil {
		t.Fatalf("could not read existing_col: %v", err)
	}
	if viaNew != "updated-via-new" {
		t.Errorf("expected UPDATE via new column to sync to existing_col, got %q", viaNew)
	}
}

// TestRollback_RenameColumn_DropsNewColumnAndTrigger verifies rollback
// works even well after COMPLETED (same reasoning as ADD_INDEX/SET_NOT_NULL:
// the new column and trigger are purely additive, so removing them never
// endangers the original column's data), and leaves the old column
// completely untouched.
func TestRollback_RenameColumn_DropsNewColumnAndTrigger(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "ddlflow_rename_rollback_test"
	createTestTable(t, pool, tableName, 3)

	store := newTestStore(t)
	flow := ddlflow.New(pool, store)

	job := &state.Job{
		ID: "rename-rollback-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhasePreflight,
		Operation: "RENAME_COLUMN", ColumnName: "existing_col", NewColumnName: "renamed_col",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}
	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Fatalf("expected Phase=COMPLETED before testing rollback-after-completion, got %s", job.Phase)
	}

	if err := flow.Rollback(ctx, job); err != nil {
		t.Fatalf("Rollback failed (should succeed even after COMPLETED for RENAME_COLUMN): %v", err)
	}
	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED after rollback, got %s", job.Phase)
	}

	var newColExists bool
	colQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = 'renamed_col')`
	if err := pool.QueryRow(ctx, colQuery, tableName).Scan(&newColExists); err != nil {
		t.Fatalf("column check failed: %v", err)
	}
	if newColExists {
		t.Error("expected the new column to have been dropped by rollback")
	}

	var oldColExists bool
	oldColQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = 'existing_col')`
	if err := pool.QueryRow(ctx, oldColQuery, tableName).Scan(&oldColExists); err != nil {
		t.Fatalf("old column check failed: %v", err)
	}
	if !oldColExists {
		t.Error("expected the old column to remain completely untouched by rollback")
	}

	var rowCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, tableName)).Scan(&rowCount); err != nil {
		t.Fatalf("could not count rows: %v", err)
	}
	if rowCount != 3 {
		t.Errorf("expected the original 3 rows to be preserved, got %d", rowCount)
	}
}

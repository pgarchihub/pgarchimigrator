//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/reaper/... -tags=integration -v
package reaper_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/ddlflow"
	"github.com/pgarchihub/pgarchimigrator/internal/reaper"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
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
	dbPath := t.TempDir() + "/reaper-test.db"
	store, err := state.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("could not create state store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestScanOnce_DropsOrphanSlotAndShadowTable sets up an end-to-end orphan
// scenario: creates a real replication slot + a real shadow table, marks a
// job referencing them as stale, then verifies ScanOnce cleans up both and
// marks the job ABORTED.
func TestScanOnce_DropsOrphanSlotAndShadowTable(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	slotName := "pgam_reaper_test_slot"
	shadowTable := "__pgam_shadow_reaper_test"

	// Clean up any leftovers from a previous run so re-running the test doesn't conflict.
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slotName)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.`+shadowTable)

	if _, err := pool.Exec(ctx, `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, slotName); err != nil {
		t.Fatalf("could not create test slot: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE public.`+shadowTable+` (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatalf("could not create test shadow table: %v", err)
	}

	store := newTestStore(t)
	job := &state.Job{
		ID:                  "orphan-job-1",
		SchemaName:          "public",
		TableName:           "orders",
		Strategy:            "SHADOW_TABLE",
		Phase:               state.PhaseDeltaSync, // "stuck" in a non-terminal phase
		ReplicationSlotName: slotName,
		ShadowTableName:     shadowTable,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	r := reaper.New(store, pool)
	r.StaleThreshold = 0 // make it stale immediately for this test

	result, err := r.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce failed: %v (result: %+v)", err, result)
	}

	if result.JobsScanned != 1 {
		t.Errorf("expected 1 job to be scanned, got %d", result.JobsScanned)
	}
	if len(result.SlotsDropped) != 1 || result.SlotsDropped[0] != slotName {
		t.Errorf("expected the slot to be dropped: %+v", result.SlotsDropped)
	}
	if len(result.ShadowTablesDropped) != 1 || result.ShadowTablesDropped[0] != shadowTable {
		t.Errorf("expected the shadow table to be dropped: %+v", result.ShadowTablesDropped)
	}

	// Verify against the DB that the slot is actually gone.
	var slotExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, slotName).Scan(&slotExists); err != nil {
		t.Fatalf("slot check failed: %v", err)
	}
	if slotExists {
		t.Error("slot still exists, was not cleaned up")
	}

	// Verify the job was marked ABORTED.
	got, err := store.Get(ctx, "orphan-job-1")
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED, got %s", got.Phase)
	}
}

// TestScanOnce_DropsOrphanBackfillIndex mirrors
// TestScanOnce_DropsOrphanSlotAndShadowTable's end-to-end shape for the
// OTHER kind of orphan an EXPAND_BACKFILL job can leave behind: a
// temporary partial index (see internal/ddlflow's createBackfillIndex
// doc comment) left over from a crash/kill mid-backfill, before its
// `defer` got a chance to drop it.
func TestScanOnce_DropsOrphanBackfillIndex(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "reaper_backfill_index_test"
	indexName := ddlflow.BackfillIndexPrefix + "created_ts_orphanjob1"

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tableName)
	if _, err := pool.Exec(ctx, `CREATE TABLE `+tableName+` (id BIGINT PRIMARY KEY, created_ts TIMESTAMPTZ)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+tableName) })
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX %s ON %s (created_ts) WHERE created_ts IS NULL`, indexName, tableName)); err != nil {
		t.Fatalf("could not create test backfill index: %v", err)
	}

	store := newTestStore(t)
	job := &state.Job{
		ID: "orphan-backfill-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhaseSyncing, // "stuck" mid-backfill
		Operation: "ADD_COLUMN", ColumnName: "created_ts",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	r := reaper.New(store, pool)
	r.StaleThreshold = 0

	result, err := r.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce failed: %v (result: %+v)", err, result)
	}

	if len(result.BackfillIndexesDropped) != 1 || result.BackfillIndexesDropped[0] != indexName {
		t.Errorf("expected the orphaned backfill index to be dropped: %+v", result.BackfillIndexesDropped)
	}

	var indexExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, indexName).Scan(&indexExists); err != nil {
		t.Fatalf("index check failed: %v", err)
	}
	if indexExists {
		t.Error("backfill index still exists, was not cleaned up")
	}

	got, err := store.Get(ctx, "orphan-backfill-job-1")
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED, got %s", got.Phase)
	}
}

// TestScanOnce_DoesNotTouchOtherTablesBackfillIndexes proves the cleanup
// is scoped to the STALE JOB's own table — a backfill index belonging to
// some OTHER, unrelated table (e.g. a different migration that's
// currently healthy and actively running) must never be touched just
// because it happens to share the BackfillIndexPrefix.
func TestScanOnce_DoesNotTouchOtherTablesBackfillIndexes(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	staleTable := "reaper_backfill_scope_stale_test"
	unrelatedTable := "reaper_backfill_scope_unrelated_test"
	unrelatedIndexName := ddlflow.BackfillIndexPrefix + "amount_unrelatedjob"

	for _, tbl := range []string{staleTable, unrelatedTable} {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl)
		if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount NUMERIC)`, tbl)); err != nil {
			t.Fatalf("could not create test table %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+staleTable)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+unrelatedTable)
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX %s ON %s (amount) WHERE amount IS NULL`, unrelatedIndexName, unrelatedTable)); err != nil {
		t.Fatalf("could not create the unrelated table's backfill index: %v", err)
	}

	store := newTestStore(t)
	job := &state.Job{
		ID: "stale-job-no-index-1", SchemaName: "public", TableName: staleTable,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhaseSyncing,
		Operation: "ADD_COLUMN", ColumnName: "amount",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	r := reaper.New(store, pool)
	r.StaleThreshold = 0

	result, err := r.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce failed: %v (result: %+v)", err, result)
	}
	if len(result.BackfillIndexesDropped) != 0 {
		t.Errorf("expected nothing dropped (the stale job's own table has no backfill index), got %+v", result.BackfillIndexesDropped)
	}

	var indexExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, unrelatedIndexName).Scan(&indexExists); err != nil {
		t.Fatalf("index check failed: %v", err)
	}
	if !indexExists {
		t.Error("the UNRELATED table's backfill index was dropped — cleanup must be scoped to the stale job's own table")
	}
}

// TestScanOnce_SkipsFreshJobs verifies that jobs that have not yet exceeded
// StaleThreshold (still "fresh") are left untouched — relies on the
// ListStale filter in reaper.go.
func TestScanOnce_SkipsFreshJobs(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	store := newTestStore(t)

	job := &state.Job{
		ID:         "fresh-job-1",
		SchemaName: "public",
		TableName:  "orders",
		Strategy:   "SHADOW_TABLE",
		Phase:      state.PhaseDeltaSync,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	r := reaper.New(store, pool)
	r.StaleThreshold = 30 * time.Minute // default; the job was just created

	result, err := r.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce failed: %v", err)
	}
	if result.JobsScanned != 0 {
		t.Errorf("expected 0 jobs to be scanned (still fresh), got %d", result.JobsScanned)
	}

	got, err := store.Get(ctx, "fresh-job-1")
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseDeltaSync {
		t.Errorf("job should not have been touched, Phase should still be DELTA_SYNC, got %s", got.Phase)
	}
}

// TestSweepExpiredRollbackWindows_CompletesExpiredJob sets up a real
// post-swap scenario: a temp table (holding the pre-swap snapshot), a
// replication slot, and a publication — exactly what ShadowFlow.Execute
// leaves behind after a successful swap — attached to a job whose
// RollbackDeadline has already passed, and verifies the sweep drops all
// three and marks the job COMPLETED (not ABORTED — this was a successful
// migration).
func TestSweepExpiredRollbackWindows_CompletesExpiredJob(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	jobID := "sweep-expired-1"
	tableName := "sweep_test_table"
	_, tempTable, slotName, pubName := shadowflow.ResourceNames(jobID, tableName)

	// Clean up any leftovers from a previous run.
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, pubName))
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slotName)
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tempTable))

	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY)`, tempTable)); err != nil {
		t.Fatalf("could not create temp table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE %s`, pubName, tempTable)); err != nil {
		t.Fatalf("could not create publication: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, slotName); err != nil {
		t.Fatalf("could not create replication slot: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tempTable))
	})

	store := newTestStore(t)
	expiredDeadline := time.Now().Add(-1 * time.Minute) // already passed
	job := &state.Job{
		ID: jobID, SchemaName: "public", TableName: tableName,
		Strategy: "SHADOW_TABLE", Phase: state.PhaseRollbackWindow,
		RollbackDeadline: &expiredDeadline,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	r := reaper.New(store, pool)
	result, err := r.SweepExpiredRollbackWindows(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredRollbackWindows failed: %v (result: %+v)", err, result)
	}
	if result.JobsSwept != 1 {
		t.Errorf("expected 1 job to be swept, got %d", result.JobsSwept)
	}

	var tempExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tempTable).Scan(&tempExists); err != nil {
		t.Fatalf("temp table check failed: %v", err)
	}
	if tempExists {
		t.Error("expected the temp table to be dropped")
	}

	var slotExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, slotName).Scan(&slotExists); err != nil {
		t.Fatalf("slot check failed: %v", err)
	}
	if slotExists {
		t.Error("expected the replication slot to be dropped")
	}

	got, err := store.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED (a successful migration, not an orphan), got %s", got.Phase)
	}
}

// TestSweepExpiredRollbackWindows_SkipsUnexpiredWindow verifies a job
// whose RollbackDeadline has NOT yet passed is left completely untouched —
// the user should still be able to call Rollback on it.
func TestSweepExpiredRollbackWindows_SkipsUnexpiredWindow(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	store := newTestStore(t)

	futureDeadline := time.Now().Add(10 * time.Minute)
	job := &state.Job{
		ID: "sweep-future-1", SchemaName: "public", TableName: "sweep_future_table",
		Strategy: "SHADOW_TABLE", Phase: state.PhaseRollbackWindow,
		RollbackDeadline: &futureDeadline,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	r := reaper.New(store, pool)
	result, err := r.SweepExpiredRollbackWindows(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredRollbackWindows failed: %v", err)
	}
	if result.JobsSwept != 0 {
		t.Errorf("expected 0 jobs to be swept (window not yet expired), got %d", result.JobsSwept)
	}

	got, err := store.Get(ctx, "sweep-future-1")
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseRollbackWindow {
		t.Errorf("job should not have been touched, expected Phase=ROLLBACK_WINDOW, got %s", got.Phase)
	}
}

// TestSweepExpiredRollbackWindows_FinalizesDropColumn verifies the
// DROP_COLUMN-specific finalization path: once the window expires, the
// reaper must actually, irreversibly drop the deprecated column — this is
// the second phase of internal/ddlflow's two-phase drop (see
// DDLFlow.executeDropColumn), and it must NOT go through the
// shadow-table-specific cleanup path (no temp table/slot/publication
// exist for a DDLFlow job, so that path would be a silent no-op if
// mistakenly taken instead).
func TestSweepExpiredRollbackWindows_FinalizesDropColumn(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	tableName := "sweep_dropcolumn_table"
	deprecatedName := "__pgam_dropped_status_abc123"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, %s TEXT)
	`, tableName, quoteTestIdent(deprecatedName))); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	})

	store := newTestStore(t)
	expiredDeadline := time.Now().Add(-1 * time.Minute)
	job := &state.Job{
		ID: "sweep-dropcol-1", SchemaName: "public", TableName: tableName,
		Strategy: "DIRECT_DDL", Phase: state.PhaseRollbackWindow,
		Operation: "DROP_COLUMN", ColumnName: "status",
		DeprecatedColumnName: deprecatedName,
		RollbackDeadline:     &expiredDeadline,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	r := reaper.New(store, pool)
	result, err := r.SweepExpiredRollbackWindows(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredRollbackWindows failed: %v (result: %+v)", err, result)
	}
	if result.JobsSwept != 1 {
		t.Errorf("expected 1 job to be swept, got %d", result.JobsSwept)
	}

	var colExists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`
	if err := pool.QueryRow(ctx, checkQuery, tableName, deprecatedName).Scan(&colExists); err != nil {
		t.Fatalf("column check failed: %v", err)
	}
	if colExists {
		t.Error("expected the deprecated column to have actually been dropped by the sweep")
	}

	got, err := store.Get(ctx, "sweep-dropcol-1")
	if err != nil {
		t.Fatalf("could not read job: %v", err)
	}
	if got.Phase != state.PhaseCompleted {
		t.Errorf("expected Phase=COMPLETED (a successful drop, not an orphan), got %s", got.Phase)
	}
}

// quoteTestIdent is a minimal identifier quoter for building test SQL —
// the deprecated column names used in these tests are always
// test-controlled, safe strings, so this is deliberately simpler than
// production quoting logic.
func quoteTestIdent(ident string) string {
	return `"` + ident + `"`
}

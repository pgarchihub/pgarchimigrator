//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/shadowflow/... -tags=integration -v -run TestExecute -timeout 60s
//	go test ./internal/shadowflow/... -tags=integration -v -run TestRollback -timeout 60s
package shadowflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

func newTestStore(t *testing.T) state.Store {
	t.Helper()
	dbPath := t.TempDir() + "/shadowflow-test.db"
	store, err := state.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("could not create state store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestExecute_EndToEnd_AlterColumnTypeSucceeds runs the full shadow-table
// flow for the architecture's flagship scenario end to end: Preflight,
// Preparation, concurrent Initial+Delta Sync (including a write generated
// DURING the run to prove Delta Sync catches it), Validation, and Swap —
// landing the job in ROLLBACK_WINDOW with the converted data live under
// the original table name.
func TestExecute_EndToEnd_AlterColumnTypeSucceeds(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_full_source"
	jobID := "e2e-full-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, sourceTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)
	`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, amount) VALUES (1, '100'), (2, '200')
	`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s, %s`, sourceTable, names.shadowTable, names.tempTable))
	})

	store := newTestStore(t)
	job := &state.Job{
		ID: jobID, SchemaName: "public", TableName: sourceTable,
		Strategy: "SHADOW_TABLE", Phase: state.PhasePreflight,
		Operation: "ALTER_COLUMN_TYPE", ColumnName: "amount", ColumnType: "integer",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	preflighter := db.NewPgxPreflighter(pool)
	flow := shadowflow.New(pool, shadowflow.ReplicationDSN(logicalDSN), store, preflighter)

	// Generate a write WHILE Execute is running, to prove Delta Sync
	// actually catches it (not just Initial Sync's static snapshot).
	go func() {
		time.Sleep(1 * time.Second)
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`INSERT INTO %s (id, amount) VALUES (3, '300')`, sourceTable))
	}()

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if job.Phase != state.PhaseRollbackWindow {
		t.Errorf("expected Phase=ROLLBACK_WINDOW after a successful swap, got %s", job.Phase)
	}
	if job.RollbackDeadline == nil {
		t.Error("expected RollbackDeadline to be set")
	}
	// Regression guard for a real (though previously silent — nothing
	// downstream happened to read these back off the in-memory job) bug:
	// UpdateResources persisted the slot/shadow-table names to the DATABASE
	// but never updated the in-memory job's own fields, unlike every other
	// Store.Update* call in this codebase. Found via an audit prompted by
	// the identical, user-visible version of this bug in
	// internal/ddlflow's Job.RowsProcessed.
	if job.ReplicationSlotName != names.slotName {
		t.Errorf("expected in-memory job.ReplicationSlotName=%q, got %q", names.slotName, job.ReplicationSlotName)
	}
	if job.ShadowTableName != names.shadowTable {
		t.Errorf("expected in-memory job.ShadowTableName=%q, got %q", names.shadowTable, job.ShadowTableName)
	}
	// And prove it's not just the in-memory struct disagreeing with a
	// stale database row: re-fetch the persisted job and confirm both
	// sides now agree.
	persisted, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("could not read back job: %v", err)
	}
	if persisted.ReplicationSlotName != job.ReplicationSlotName {
		t.Errorf(
			"in-memory (%q) and persisted (%q) ReplicationSlotName disagree",
			job.ReplicationSlotName, persisted.ReplicationSlotName,
		)
	}
	if persisted.ShadowTableName != job.ShadowTableName {
		t.Errorf(
			"in-memory (%q) and persisted (%q) ShadowTableName disagree",
			job.ShadowTableName, persisted.ShadowTableName,
		)
	}

	// After the swap, "sourceTable" holds the NEW (post-swap) data with the
	// converted column type.
	var amount1, amount2, amount3 int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT amount FROM %s WHERE id = 1`, sourceTable)).Scan(&amount1); err != nil {
		t.Fatalf("could not read id=1: %v", err)
	}
	if amount1 != 100 {
		t.Errorf("expected amount=100 for id=1, got %d", amount1)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT amount FROM %s WHERE id = 2`, sourceTable)).Scan(&amount2); err != nil {
		t.Fatalf("could not read id=2: %v", err)
	}
	if amount2 != 200 {
		t.Errorf("expected amount=200 for id=2, got %d", amount2)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT amount FROM %s WHERE id = 3`, sourceTable)).Scan(&amount3); err != nil {
		t.Fatalf("expected id=3 (inserted during Execute) to have been caught by Delta Sync: %v", err)
	}
	if amount3 != 300 {
		t.Errorf("expected amount=300 for id=3, got %d", amount3)
	}

	var colType string
	if err := pool.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns WHERE table_name = $1 AND column_name = 'amount'
	`, sourceTable).Scan(&colType); err != nil {
		t.Fatalf("could not check column type: %v", err)
	}
	if colType != "integer" {
		t.Errorf("expected amount column to be integer after swap, got %q", colType)
	}

	// The original (pre-swap) data must still be preserved under tempTable
	// during the rollback window. Note: the concurrent write (id=3) above
	// is generated ~1s into Execute, comfortably before the ~2s settle
	// window elapses and Swap runs — so it lands on the LIVE source table
	// before the swap, and therefore ends up as part of what gets renamed
	// to tempTable (the source table's original identity), not left behind.
	var tempCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, names.tempTable)).Scan(&tempCount); err != nil {
		t.Fatalf("expected tempTable to still exist during the rollback window: %v", err)
	}
	if tempCount != 3 {
		t.Errorf("expected 3 rows preserved in tempTable (2 seeded + 1 concurrent insert before swap), got %d", tempCount)
	}
}

// TestRollback_DuringRollbackWindow_RestoresOriginal runs Execute to
// completion, then immediately calls Rollback (well within the window) and
// verifies the original (pre-swap) data and type are restored under the
// source table's name.
func TestRollback_DuringRollbackWindow_RestoresOriginal(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_rollback_source"
	jobID := "rollback-window-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, sourceTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, amount) VALUES (1, '100')`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s, %s`, sourceTable, names.shadowTable, names.tempTable))
	})

	store := newTestStore(t)
	job := &state.Job{
		ID: jobID, SchemaName: "public", TableName: sourceTable,
		Strategy: "SHADOW_TABLE", Phase: state.PhasePreflight,
		Operation: "ALTER_COLUMN_TYPE", ColumnName: "amount", ColumnType: "integer",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	preflighter := db.NewPgxPreflighter(pool)
	flow := shadowflow.New(pool, shadowflow.ReplicationDSN(logicalDSN), store, preflighter)

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if err := flow.Rollback(ctx, job); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED after rollback, got %s", job.Phase)
	}

	// The source table name must hold the ORIGINAL (pre-swap) text data again.
	var amount string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT amount FROM %s WHERE id = 1`, sourceTable)).Scan(&amount); err != nil {
		t.Fatalf("could not read restored source table: %v", err)
	}
	if amount != "100" {
		t.Errorf("expected the original text value '100' to be restored, got %q", amount)
	}

	var colType string
	if err := pool.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns WHERE table_name = $1 AND column_name = 'amount'
	`, sourceTable).Scan(&colType); err != nil {
		t.Fatalf("could not check column type: %v", err)
	}
	if colType != "text" {
		t.Errorf("expected the amount column to be text again after rollback, got %q", colType)
	}

	// The post-swap (new) data must be preserved under the shadow-table
	// name, not silently dropped.
	var preservedCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, names.shadowTable)).Scan(&preservedCount); err != nil {
		t.Fatalf("expected the post-swap data to be preserved under the shadow-table name: %v", err)
	}
	if preservedCount != 1 {
		t.Errorf("expected 1 preserved row, got %d", preservedCount)
	}

	var slotExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, names.slotName).Scan(&slotExists); err != nil {
		t.Fatalf("slot check failed: %v", err)
	}
	if slotExists {
		t.Error("expected the replication slot to be cleaned up after rollback")
	}
}

// TestRollback_ExpiredWindow_ReturnsError verifies FR-08a: once the
// rollback deadline has passed, Rollback must refuse and leave everything
// untouched — this is meant to be surfaced to the user as "you need a new
// migration, not a rollback".
func TestRollback_ExpiredWindow_ReturnsError(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_expired_source"
	jobID := "rollback-expired-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, sourceTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, amount) VALUES (1, '100')`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s, %s`, sourceTable, names.shadowTable, names.tempTable))
	})

	store := newTestStore(t)
	job := &state.Job{
		ID: jobID, SchemaName: "public", TableName: sourceTable,
		Strategy: "SHADOW_TABLE", Phase: state.PhasePreflight,
		Operation: "ALTER_COLUMN_TYPE", ColumnName: "amount", ColumnType: "integer",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	preflighter := db.NewPgxPreflighter(pool)
	flow := shadowflow.New(pool, shadowflow.ReplicationDSN(logicalDSN), store, preflighter)
	flow.RollbackWindow = 1 * time.Millisecond // force an immediate expiry for this test

	if err := flow.Execute(ctx, job); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond) // ensure the 1ms window has definitely passed

	err := flow.Rollback(ctx, job)
	if err == nil {
		t.Fatal("expected Rollback to fail once the window has expired")
	}

	// The post-swap data must remain exactly as-is — an expired-window
	// Rollback call must not touch anything.
	var amount int
	if scanErr := pool.QueryRow(ctx, fmt.Sprintf(`SELECT amount FROM %s WHERE id = 1`, sourceTable)).Scan(&amount); scanErr != nil {
		t.Fatalf("expected the post-swap table to be untouched: %v", scanErr)
	}
	if amount != 100 {
		t.Errorf("expected amount=100 (untouched post-swap data), got %d", amount)
	}
}

// TestRollback_PreSwapPhase_CleansUpWithoutTouchingSource simulates an
// Execute call that was interrupted before the swap (e.g. a crash during
// Delta Sync) by manually creating the same partial state Execute's
// prepare() step would have left behind, then verifies Rollback cleans it
// up (shadow table, slot, publication) while leaving the original,
// never-renamed source table completely untouched.
func TestRollback_PreSwapPhase_CleansUpWithoutTouchingSource(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_presw_source"
	jobID := "rollback-presw-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, sourceTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, amount) VALUES (1, '100')`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, names.shadowTable))
	})

	// Manually recreate what prepare() would have done, to simulate a crash
	// partway through Execute (before the swap ever ran).
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING ALL)`, names.shadowTable, sourceTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	if err := shadowflow.CreatePublication(ctx, pool, "public", sourceTable, names.pubName); err != nil {
		t.Fatalf("could not create publication: %v", err)
	}
	if _, err := shadowflow.CreateReplicationSlotAndGetStartLSN(ctx, shadowflow.ReplicationDSN(logicalDSN), names.slotName); err != nil {
		t.Fatalf("could not create replication slot: %v", err)
	}

	store := newTestStore(t)
	job := &state.Job{
		ID: jobID, SchemaName: "public", TableName: sourceTable,
		Strategy: "SHADOW_TABLE", Phase: state.PhaseDeltaSync, // "interrupted" mid-flight
		Operation: "ALTER_COLUMN_TYPE", ColumnName: "amount", ColumnType: "integer",
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job: %v", err)
	}

	preflighter := db.NewPgxPreflighter(pool)
	flow := shadowflow.New(pool, shadowflow.ReplicationDSN(logicalDSN), store, preflighter)

	if err := flow.Rollback(ctx, job); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	if job.Phase != state.PhaseAborted {
		t.Errorf("expected Phase=ABORTED, got %s", job.Phase)
	}

	// The original source table must be completely untouched.
	var amount string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT amount FROM %s WHERE id = 1`, sourceTable)).Scan(&amount); err != nil {
		t.Fatalf("source table should be untouched: %v", err)
	}
	if amount != "100" {
		t.Errorf("expected the untouched original value '100', got %q", amount)
	}

	var shadowExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, names.shadowTable).Scan(&shadowExists); err != nil {
		t.Fatalf("shadow table check failed: %v", err)
	}
	if shadowExists {
		t.Error("expected the shadow table to be dropped by rollback")
	}

	var slotExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, names.slotName).Scan(&slotExists); err != nil {
		t.Fatalf("slot check failed: %v", err)
	}
	if slotExists {
		t.Error("expected the replication slot to be dropped by rollback")
	}
}

// testResourceNames mirrors shadowflow's unexported resourceNamesFor logic
// so the test can predict and clean up the exact same names Execute/Rollback
// will generate. Kept in sync manually since the real function is
// unexported by design (it's an internal implementation detail, not part
// of the package's public API).
type testNames struct {
	shadowTable string
	tempTable   string
	slotName    string
	pubName     string
}

func testResourceNames(jobID, tableName string) testNames {
	safeID := jobID
	for i := 0; i < len(safeID); i++ {
		if safeID[i] == '-' {
			safeID = safeID[:i] + "_" + safeID[i+1:]
		}
	}
	if len(safeID) > 16 {
		safeID = safeID[:16]
	}
	safeTable := tableName
	if len(safeTable) > 20 {
		safeTable = safeTable[:20]
	}
	return testNames{
		shadowTable: fmt.Sprintf("__pgam_shadow_%s_%s", safeTable, safeID),
		tempTable:   fmt.Sprintf("__pgam_temp_%s_%s", safeTable, safeID),
		slotName:    fmt.Sprintf("pgam_slot_%s_%s", safeTable, safeID),
		pubName:     fmt.Sprintf("pgam_pub_%s_%s", safeTable, safeID),
	}
}

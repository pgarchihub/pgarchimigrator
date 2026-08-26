//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/shadowflow/... -tags=integration -v -run TestExecute_Preserves -timeout 60s
//	go test ./internal/shadowflow/... -tags=integration -v -run TestExecute_Reattaches -timeout 60s
package shadowflow_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// TestExecute_PreservesTrigger verifies a user-defined trigger on the
// source table is recreated on the shadow table (disabled during sync,
// re-enabled after swap) and actually fires correctly post-migration.
func TestExecute_PreservesTrigger(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "dep_trigger_source"
	logTable := "dep_trigger_log"
	jobID := "dep-trigger-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s CASCADE`, sourceTable, logTable))
	_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS dep_trigger_fn() CASCADE`)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)
	`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (event TEXT NOT NULL)
	`, logTable)); err != nil {
		t.Fatalf("could not create log table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION dep_trigger_fn() RETURNS trigger AS $$
		BEGIN
			INSERT INTO %s (event) VALUES ('inserted:' || NEW.id);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql
	`, logTable)); err != nil {
		t.Fatalf("could not create trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER dep_trg AFTER INSERT ON %s
		FOR EACH ROW EXECUTE FUNCTION dep_trigger_fn()
	`, sourceTable)); err != nil {
		t.Fatalf("could not create trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, '100')`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s, %s, %s CASCADE`, sourceTable, names.shadowTable, names.tempTable, logTable))
		_, _ = pool.Exec(bg, `DROP FUNCTION IF EXISTS dep_trigger_fn() CASCADE`)
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

	// The seed insert above (before Execute was even called) already fired
	// the ORIGINAL trigger on the source table normally — that's expected,
	// unrelated to migration mechanics, and produces exactly 1 log entry.
	// What we're actually verifying here is that the shadow table's OWN
	// copy of the trigger stayed silent throughout Initial Sync / Delta
	// Sync (it should have been disabled) — i.e. the count should NOT have
	// grown beyond that single pre-existing entry as a side effect of sync.
	var preSwapLogCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, logTable)).Scan(&preSwapLogCount); err != nil {
		t.Fatalf("could not count log rows: %v", err)
	}
	if preSwapLogCount != 1 {
		t.Errorf("expected exactly 1 log entry (from the pre-Execute seed insert only — the shadow table's disabled trigger copy should not have added more during sync), got %d", preSwapLogCount)
	}

	// Now insert into the (post-swap) live table and verify the trigger fires.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (2, '200')`, sourceTable)); err != nil {
		t.Fatalf("could not insert post-swap: %v", err)
	}

	var postSwapLogCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE event = 'inserted:2'`, logTable)).Scan(&postSwapLogCount); err != nil {
		t.Fatalf("could not count log rows: %v", err)
	}
	if postSwapLogCount != 1 {
		t.Errorf("expected the trigger to fire exactly once for the post-swap insert, got %d matching log entries", postSwapLogCount)
	}

	// Total should be exactly 2 (1 from the pre-Execute seed insert, 1 from
	// the post-swap insert) — proving the trigger never double-fired during
	// the Initial Sync / Delta Sync overlap window.
	var totalLogCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, logTable)).Scan(&totalLogCount); err != nil {
		t.Fatalf("could not count total log rows: %v", err)
	}
	if totalLogCount != 2 {
		t.Errorf("expected exactly 2 total log entries, got %d (possible double-firing during sync)", totalLogCount)
	}
}

// TestExecute_ReattachesForeignKey verifies a foreign key from another
// table, referencing the migrated table, is preserved and actually
// enforced after the swap.
func TestExecute_ReattachesForeignKey(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "dep_fk_parent"
	childTable := "dep_fk_child"
	jobID := "dep-fk-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s CASCADE`, childTable, sourceTable))

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)
	`, sourceTable)); err != nil {
		t.Fatalf("could not create parent table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, parent_id BIGINT REFERENCES %s(id))
	`, childTable, sourceTable)); err != nil {
		t.Fatalf("could not create child table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, '100')`, sourceTable)); err != nil {
		t.Fatalf("could not seed parent table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 1)`, childTable)); err != nil {
		t.Fatalf("could not seed child table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s, %s, %s CASCADE`, childTable, sourceTable, names.shadowTable, names.tempTable))
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

	// The FK must still be enforced: inserting a child row with a
	// non-existent parent_id must fail.
	_, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (2, 999)`, childTable))
	if err == nil {
		t.Error("expected the foreign key to reject a reference to a non-existent parent row")
	}

	// And a valid reference must still succeed.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (3, 1)`, childTable)); err != nil {
		t.Errorf("expected a valid foreign key reference to succeed, got: %v", err)
	}
}

// TestExecute_ReattachesSequenceOwnership verifies a classic SERIAL column
// keeps working (via nextval) after the migration, and that its sequence
// survives Cleanup (i.e. it was re-pointed to the shadow table, not left
// owned by the dropped temp table).
func TestExecute_ReattachesSequenceOwnership(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "dep_serial_source"
	jobID := "dep-serial-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, sourceTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id SERIAL PRIMARY KEY, amount TEXT NOT NULL)
	`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (amount) VALUES ('100')`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s, %s CASCADE`, sourceTable, names.shadowTable, names.tempTable))
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

	// Simulate Cleanup (dropping tempTable) to prove the sequence
	// survives — this is exactly what would CASCADE-drop the sequence if
	// ownership had not been re-pointed to the shadow table before the
	// swap.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, names.tempTable)); err != nil {
		t.Fatalf("could not drop temp table: %v", err)
	}

	var newID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`INSERT INTO %s (amount) VALUES ('200') RETURNING id`, sourceTable)).Scan(&newID); err != nil {
		t.Fatalf("expected the SERIAL column to still work after dropping the temp table, got: %v", err)
	}
	if newID <= 1 {
		t.Errorf("expected a fresh, incrementing id, got %d", newID)
	}
}

// TestRevertSequenceOwnership_AllowsCleanShadowTableDrop is the direct
// regression test for a real bug found via manual investigation of an
// orphaned shadow table `internal/reaper` could never clean up.
//
// ApplyToShadowTable's sequence-ownership transfer (exercised by
// TestExecute_ReattachesSequenceOwnership above) runs during Preparation,
// well before Initial Sync/Validation/Swap — so if a migration fails
// AFTER that point (initial sync failing, validation failing, a crash),
// the shadow table is left "owning" a sequence the LIVE source table's
// own column DEFAULT still depends on. A plain `DROP TABLE` on the shadow
// table then fails with a real PostgreSQL error (SQLSTATE 2BP01,
// "cannot drop table ... because other objects depend on it") — and
// CASCADE is not a safe fix: it would drop the sequence entirely,
// stripping the auto-increment default off the LIVE, still-in-production
// source table. RevertSequenceOwnership (called by failAndCleanup before
// its own DROP TABLE) re-points ownership back to the source table first,
// so the shadow table can be dropped cleanly.
//
// This test reproduces the exact failure mode directly — set up the
// broken state, confirm the plain DROP TABLE genuinely fails (so the test
// itself isn't a false positive), then confirm RevertSequenceOwnership
// fixes it.
func TestRevertSequenceOwnership_AllowsCleanShadowTableDrop(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "dep_revertseq_source"
	shadowTable := "dep_revertseq_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s CASCADE`, sourceTable, shadowTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id SERIAL PRIMARY KEY, amount TEXT NOT NULL)
	`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING ALL)`, shadowTable, sourceTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s CASCADE`, sourceTable, shadowTable))
	})

	deps, err := shadowflow.Inventory(ctx, pool, "public", sourceTable)
	if err != nil {
		t.Fatalf("could not inventory dependent objects: %v", err)
	}

	// Simulates prepare()'s call during a real migration's Preparation phase.
	if err := shadowflow.ApplyToShadowTable(ctx, pool, "public", shadowTable, deps); err != nil {
		t.Fatalf("could not apply dependent objects to shadow table: %v", err)
	}

	// Confirm the bug is actually reproduced here — a plain DROP TABLE
	// must genuinely fail, or the rest of this test wouldn't be proving
	// anything.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, shadowTable)); err == nil {
		t.Fatal("expected the plain DROP TABLE to fail (sequence still owned by the shadow table) — " +
			"if it succeeded, this test no longer reproduces the bug it's meant to guard against")
	} else {
		t.Logf("got the expected DROP TABLE failure (good — confirms the bug is reproduced here): %v", err)
	}

	if err := shadowflow.RevertSequenceOwnership(ctx, pool, "public", sourceTable, deps); err != nil {
		t.Fatalf("RevertSequenceOwnership failed: %v", err)
	}

	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, shadowTable)); err != nil {
		t.Fatalf("expected DROP TABLE to succeed after RevertSequenceOwnership, got: %v", err)
	}

	// The whole point: the LIVE source table's auto-increment must still
	// work correctly after all this — RevertSequenceOwnership must not
	// have broken the very thing it was protecting.
	var newID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`INSERT INTO %s (amount) VALUES ('after-revert') RETURNING id`, sourceTable)).Scan(&newID); err != nil {
		t.Fatalf("expected the source table's SERIAL column to still work, got: %v", err)
	}
	if newID <= 0 {
		t.Errorf("expected a valid auto-incremented id, got %d", newID)
	}
}

// TestRollback_ReattachesForeignKey verifies the symmetric fix: a foreign
// key from another table survives not just Execute's swap, but ALSO a
// subsequent Rollback's reverse swap — it must still enforce referential
// integrity against the RESTORED original table.
func TestRollback_ReattachesForeignKey(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "dep_rollback_fk_parent"
	childTable := "dep_rollback_fk_child"
	jobID := "dep-rollback-fk-1"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s CASCADE`, childTable, sourceTable))

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)
	`, sourceTable)); err != nil {
		t.Fatalf("could not create parent table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, parent_id BIGINT REFERENCES %s(id))
	`, childTable, sourceTable)); err != nil {
		t.Fatalf("could not create child table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, '100')`, sourceTable)); err != nil {
		t.Fatalf("could not seed parent table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 1)`, childTable)); err != nil {
		t.Fatalf("could not seed child table: %v", err)
	}

	names := testResourceNames(jobID, sourceTable)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, names.pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, names.slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s, %s, %s CASCADE`, childTable, sourceTable, names.shadowTable, names.tempTable))
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

	// Sanity check: the FK is enforced right after the forward swap
	// (mirrors TestExecute_ReattachesForeignKey).
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (2, 999)`, childTable)); err == nil {
		t.Fatal("expected the foreign key to reject an invalid reference before rollback")
	}

	if err := flow.Rollback(ctx, job); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	// The FK must STILL be enforced after the reverse swap, against the
	// now-restored original table.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (3, 999)`, childTable)); err == nil {
		t.Error("expected the foreign key to still reject an invalid reference after rollback")
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (4, 1)`, childTable)); err != nil {
		t.Errorf("expected a valid foreign key reference to succeed after rollback, got: %v", err)
	}
}

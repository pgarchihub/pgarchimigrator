//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/shadowflow/... -tags=integration -v -run TestEndToEnd -timeout 60s
//	go test ./internal/shadowflow/... -tags=integration -v -run TestApplyEngine
//	go test ./internal/shadowflow/... -tags=integration -v -run TestInitialSync
package shadowflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
)

type row struct {
	id   int64
	name string
}

func readAllRows(t *testing.T, pool *pgxpool.Pool, table string) map[int64]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), fmt.Sprintf(`SELECT id, name FROM %s`, table))
	if err != nil {
		t.Fatalf("could not read from %s: %v", table, err)
	}
	defer rows.Close()

	result := map[int64]string{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name); err != nil {
			t.Fatalf("could not scan row from %s: %v", table, err)
		}
		result[r.id] = r.name
	}
	return result
}

func rowsEqual(a, b map[int64]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestEndToEnd_InitialSyncPlusDeltaSync_Converges is the single most
// important test in this package: it proves the full pipeline described in
// Architecture Doc Section 4.1 steps 1-4 (Preparation, Initial Sync, Delta
// Sync, and implicitly Validation via the final comparison) actually
// converges the shadow table with the source table, including the
// "overlap window" the idempotent-apply design in replication.go is meant
// to handle safely.
func TestEndToEnd_InitialSyncPlusDeltaSync_Converges(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_e2e_source"
	shadowTable := "shadowflow_e2e_shadow"
	slotName := "pgam_e2e_slot"
	pubName := "pgam_e2e_pub"

	// Clean up any leftovers from a previous run.
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, pubName))
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slotName)
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, name TEXT NOT NULL)
	`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING ALL)`, shadowTable, sourceTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s`, pubName))
		_, _ = pool.Exec(bg, `SELECT pg_drop_replication_slot($1)`, slotName)
		_, _ = pool.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	})

	// Seed rows that exist BEFORE the slot is created — these must be
	// picked up by Initial Sync, not Delta Sync.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 'alice'), (2, 'bob')`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	if err := shadowflow.CreatePublication(ctx, pool, "public", sourceTable, pubName); err != nil {
		t.Fatalf("could not create publication: %v", err)
	}

	replicationDSN := shadowflow.ReplicationDSN(logicalDSN)
	startLSN, err := shadowflow.CreateReplicationSlotAndGetStartLSN(ctx, replicationDSN, slotName)
	if err != nil {
		t.Fatalf("could not create replication slot: %v", err)
	}

	// A row inserted AFTER the slot exists but BEFORE Initial Sync runs —
	// this exercises the overlap window the idempotent Apply design exists
	// to handle: it will be picked up by BOTH Initial Sync and Delta Sync.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (3, 'carol')`, sourceTable)); err != nil {
		t.Fatalf("could not insert pre-sync row: %v", err)
	}

	pkCols, err := shadowflow.PrimaryKeyColumns(ctx, pool, "public", sourceTable)
	if err != nil {
		t.Fatalf("could not get primary key columns: %v", err)
	}

	syncCfg := shadowflow.InitialSyncConfig{
		Pool: pool, BatchSize: 2,
		SourceSchema: "public", SourceTable: sourceTable, ShadowTable: shadowTable, PKColumns: pkCols,
	}
	if err := syncCfg.Run(ctx); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	engine := &shadowflow.SyncEngine{
		Decoder: &shadowflow.Decoder{ReplicationDSN: replicationDSN, SlotName: slotName, PublicationName: pubName},
		Apply:   &shadowflow.ApplyEngine{Pool: pool, Schema: "public", ShadowTable: shadowTable, PrimaryKeyColumns: pkCols},
	}

	syncCtx, cancelSync := context.WithCancel(context.Background())
	defer cancelSync()
	syncErrCh := make(chan error, 1)
	go func() {
		syncErrCh <- engine.Run(syncCtx, startLSN)
	}()

	// Give the replication connection a moment to start streaming before
	// generating more changes.
	time.Sleep(500 * time.Millisecond)

	// Exercise INSERT, UPDATE, DELETE through Delta Sync.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (4, 'dave')`, sourceTable)); err != nil {
		t.Fatalf("could not insert: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET name = 'alice-updated' WHERE id = 1`, sourceTable)); err != nil {
		t.Fatalf("could not update: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = 2`, sourceTable)); err != nil {
		t.Fatalf("could not delete: %v", err)
	}

	// Poll until the shadow table converges with the source table, or time out.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for convergence; source=%v shadow=%v",
				readAllRows(t, pool, sourceTable), readAllRows(t, pool, shadowTable))
		}

		sourceRows := readAllRows(t, pool, sourceTable)
		shadowRows := readAllRows(t, pool, shadowTable)
		if rowsEqual(sourceRows, shadowRows) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	cancelSync()
	select {
	case err := <-syncErrCh:
		if err != nil && err != context.Canceled {
			t.Errorf("SyncEngine.Run returned an unexpected error after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("SyncEngine.Run did not stop after context cancellation")
	}
}

// TestApplyEngine_ExplicitCast_TextToInteger verifies the flagship
// shadow-table scenario from Architecture Doc Section 4.0 — an
// incompatible type conversion — is handled correctly by ApplyEngine's
// CastColumn/CastType mechanism, since pgoutput always delivers values as
// text and text->integer is not an assignment-compatible cast in
// PostgreSQL.
func TestApplyEngine_ExplicitCast_TextToInteger(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	shadowTable := "shadowflow_cast_test"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, shadowTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount INTEGER)`, shadowTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, shadowTable)) })

	engine := &shadowflow.ApplyEngine{
		Pool: pool, Schema: "public", ShadowTable: shadowTable,
		PrimaryKeyColumns: []string{"id"},
		CastColumn:        "amount",
		CastType:          "integer",
	}

	event := shadowflow.ChangeEvent{
		Kind:    shadowflow.ChangeInsert,
		Schema:  "public",
		Table:   "source",
		Columns: []string{"id", "amount"},
		Values:  []any{"1", "12345"}, // pgoutput always delivers text-encoded values
	}
	if err := engine.Apply(ctx, event); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	var amount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT amount FROM %s WHERE id = 1`, shadowTable)).Scan(&amount); err != nil {
		t.Fatalf("could not read back: %v", err)
	}
	if amount != 12345 {
		t.Errorf("expected amount=12345, got %d", amount)
	}
}

// TestApplyEngine_Delete_RemovesRowByPrimaryKey verifies the DELETE path
// independent of the end-to-end streaming test above.
func TestApplyEngine_Delete_RemovesRowByPrimaryKey(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	shadowTable := "shadowflow_delete_test"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, shadowTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, name TEXT)`, shadowTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, shadowTable)) })

	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 'to-be-deleted')`, shadowTable)); err != nil {
		t.Fatalf("could not seed shadow table: %v", err)
	}

	engine := &shadowflow.ApplyEngine{
		Pool: pool, Schema: "public", ShadowTable: shadowTable,
		PrimaryKeyColumns: []string{"id"},
	}

	event := shadowflow.ChangeEvent{
		Kind:    shadowflow.ChangeDelete,
		Schema:  "public",
		Table:   "source",
		Columns: []string{"id", "name"},
		Values:  []any{"1", "to-be-deleted"},
	}
	if err := engine.Apply(ctx, event); err != nil {
		t.Fatalf("Apply (delete) failed: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, shadowTable)).Scan(&count); err != nil {
		t.Fatalf("could not count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected the row to be deleted, %d row(s) remain", count)
	}
}

// TestInitialSync_MultipleBatches_CopiesAllRows verifies the resumable
// ctid-based batching in initial_sync.go actually walks through more than
// one batch and copies every row exactly once.
func TestInitialSync_MultipleBatches_CopiesAllRows(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_initsync_source"
	shadowTable := "shadowflow_initsync_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, name TEXT NOT NULL)`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING ALL)`, shadowTable, sourceTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, name) SELECT g, 'row' || g FROM generate_series(1, 250) g
	`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	cfg := shadowflow.InitialSyncConfig{
		Pool: pool, BatchSize: 100, // 250 rows / 100 per batch = 3 rounds
		SourceSchema: "public", SourceTable: sourceTable, ShadowTable: shadowTable,
		PKColumns: []string{"id"}, // matches this test's own `id BIGINT PRIMARY KEY` above
	}
	if err := cfg.Run(ctx); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, shadowTable)).Scan(&count); err != nil {
		t.Fatalf("could not count shadow rows: %v", err)
	}
	if count != 250 {
		t.Errorf("expected all 250 rows to be copied, got %d", count)
	}
}

// TestInitialSync_OnBatchCompleteReportsAccurateProgress is the direct
// regression test for a real observability gap this whole feature closes:
// SHADOW_TABLE migrations previously gave ZERO progress visibility while
// running — a genuinely large table taking a long time looked, from the
// outside (API, web dashboard, or a direct database query), identical to
// one that was permanently stuck. Found the hard way, diagnosing a real
// multi-hour SYNCING phase that needed direct pg_stat_activity/
// pg_replication_slots inspection because nothing in this project's own
// observability surfaced it.
//
// Verifies both that OnBatchComplete fires once per batch (3 rounds for
// 250 rows ÷ 100 per batch) AND that the SUM across all calls equals the
// true total — a naive per-batch-size assumption would be wrong here
// specifically because ON CONFLICT DO NOTHING (see runBatch's doc
// comment) means a batch's reported count is the rows actually inserted,
// not the batch size.
func TestInitialSync_OnBatchCompleteReportsAccurateProgress(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_initsync_progress_source"
	shadowTable := "shadowflow_initsync_progress_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, name TEXT NOT NULL)`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING ALL)`, shadowTable, sourceTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, name) SELECT g, 'row' || g FROM generate_series(1, 250) g
	`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	// Rows 1-30 already present, simulating some already having been
	// applied via delta sync — exercises the exact scenario that makes
	// per-batch counts (not just per-batch size) matter: batch 1 (rows
	// 1-100) should report 70 actually inserted, not 100.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, name) SELECT g, 'row' || g FROM generate_series(1, 30) g
	`, shadowTable)); err != nil {
		t.Fatalf("could not pre-seed the shadow table: %v", err)
	}

	var callCount int
	var totalReported int64
	cfg := shadowflow.InitialSyncConfig{
		Pool: pool, BatchSize: 100, // 250 rows / 100 per batch = 3 rounds
		SourceSchema: "public", SourceTable: sourceTable, ShadowTable: shadowTable,
		PKColumns: []string{"id"},
		OnBatchComplete: func(rowsCopiedThisBatch int64) {
			callCount++
			totalReported += rowsCopiedThisBatch
		},
	}
	if err := cfg.Run(ctx); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	if callCount != 3 {
		t.Errorf("expected OnBatchComplete to fire once per batch (3 rounds), got %d calls", callCount)
	}
	// 250 total rows - 30 pre-existing = 220 actually inserted by this run.
	if totalReported != 220 {
		t.Errorf("expected the sum of all OnBatchComplete calls to equal 220 (250 rows - 30 pre-existing), got %d", totalReported)
	}
}

// TestInitialSync_RowAlreadyInShadowTable_SkippedNotErrored is the direct
// regression test for a real bug found via load testing (concurrent write
// traffic during a real SHADOW_TABLE migration): this package's own
// design deliberately runs delta-sync (logical replication capture +
// ApplyEngine, which upserts) CONCURRENTLY with initial sync's own
// ctid-ordered batch copy (see shadowflow.go's startDeltaSync being
// launched before runInitialSync) — if ApplyEngine processes a
// concurrently-replicated change and inserts a row into the shadow table
// BEFORE initial sync's own scan reaches that same row, initial sync's
// batch INSERT used to fail with a real
// "duplicate key value violates unique constraint ... (SQLSTATE 23505)"
// once it got there too.
//
// This test doesn't try to reproduce the exact timing race (unreliable,
// timing-dependent) — it directly tests the invariant the fix
// establishes: a row ApplyEngine already wrote into the shadow table
// (simulated here by inserting it manually, standing in for
// ApplyEngine.upsert having already run) must not cause initial sync to
// fail when its own scan reaches that same row, and — just as
// important — must not be overwritten by initial sync's older,
// snapshot-based copy of that row (ApplyEngine's version, from the live
// replication stream, is definitionally not older).
func TestInitialSync_RowAlreadyInShadowTable_SkippedNotErrored(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_initsync_conflict_source"
	shadowTable := "shadowflow_initsync_conflict_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, name TEXT NOT NULL)`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING ALL)`, shadowTable, sourceTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, name) SELECT g, 'original-row' || g FROM generate_series(1, 50) g
	`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	// Simulates ApplyEngine.upsert already having processed a
	// concurrently-replicated change for row id=25, BEFORE initial sync's
	// own scan gets to it — deliberately a DIFFERENT value than the
	// source table's row 25, so the test can later confirm initial sync
	// did NOT clobber it.
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, name) VALUES (25, 'applied-by-delta-sync-already')`, shadowTable,
	)); err != nil {
		t.Fatalf("could not pre-seed the conflicting shadow row: %v", err)
	}

	cfg := shadowflow.InitialSyncConfig{
		Pool: pool, BatchSize: 10, // small batches so row 25 isn't in the very first batch
		SourceSchema: "public", SourceTable: sourceTable, ShadowTable: shadowTable,
		PKColumns: []string{"id"},
	}
	if err := cfg.Run(ctx); err != nil {
		t.Fatalf("expected initial sync to succeed (skipping the already-present row), got: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, shadowTable)).Scan(&count); err != nil {
		t.Fatalf("could not count shadow rows: %v", err)
	}
	if count != 50 {
		t.Errorf("expected all 50 rows present (49 copied + 1 pre-existing), got %d", count)
	}

	var name string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s WHERE id = 25`, shadowTable)).Scan(&name); err != nil {
		t.Fatalf("could not read row 25: %v", err)
	}
	if name != "applied-by-delta-sync-already" {
		t.Errorf("expected initial sync to leave the pre-existing (delta-sync-applied) row untouched, "+
			"but it was overwritten with %q — ON CONFLICT DO NOTHING should have made this a no-op", name)
	}
}

// TestInitialSync_EntireBatchAlreadyPresent_DoesNotTerminateEarly is the
// direct regression test for a second, more subtle bug introduced by the
// ON CONFLICT DO NOTHING fix itself (see runBatch's doc comment) and
// caught before it shipped, via a real load test: the loop used to stop
// as soon as a batch inserted ZERO rows (rowsCopied == 0), which used to
// correctly mean "nothing left to scan" — but after ON CONFLICT DO
// NOTHING, a batch can find candidate rows and still insert zero of them
// (every single one already present, e.g. under heavy concurrent write
// load where ApplyEngine's delta sync races ahead of initial sync's own
// scan for an entire batch's worth of rows, not just one). Stopping on
// rowsCopied==0 in that case would silently abandon the sync partway
// through, leaving every row past that point never copied. The fix
// checks whether the batch found any candidate ctids at all
// (newLastCtid != nil), not how many were actually inserted.
//
// This seeds an entire FIRST batch's worth of rows as already present in
// the shadow table — unlike the single-row-conflict test above, which
// wouldn't have caught this (only one row conflicting out of ten still
// left rowsCopied at 9, never 0).
func TestInitialSync_EntireBatchAlreadyPresent_DoesNotTerminateEarly(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	sourceTable := "shadowflow_initsync_wholebatch_source"
	shadowTable := "shadowflow_initsync_wholebatch_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, name TEXT NOT NULL)`, sourceTable)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING ALL)`, shadowTable, sourceTable)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, sourceTable, shadowTable))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, name) SELECT g, 'row' || g FROM generate_series(1, 30) g
	`, sourceTable)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	// Simulates ApplyEngine having already caught up on the entire FIRST
	// physical range of the table (rows 1-10, matching batch size 10
	// below) before initial sync's own scan even begins.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, name) SELECT g, 'row' || g FROM generate_series(1, 10) g
	`, shadowTable)); err != nil {
		t.Fatalf("could not pre-seed the shadow table: %v", err)
	}

	cfg := shadowflow.InitialSyncConfig{
		Pool: pool, BatchSize: 10, // batch 1 = rows 1-10, ALL already present
		SourceSchema: "public", SourceTable: sourceTable, ShadowTable: shadowTable,
		PKColumns: []string{"id"},
	}
	if err := cfg.Run(ctx); err != nil {
		t.Fatalf("expected initial sync to succeed and continue past the fully-conflicting first batch, got: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, shadowTable)).Scan(&count); err != nil {
		t.Fatalf("could not count shadow rows: %v", err)
	}
	if count != 30 {
		t.Errorf("expected all 30 rows present (10 pre-existing + 20 copied), got %d — "+
			"if this is 10, the sync stopped early after the fully-conflicting first batch", count)
	}
}

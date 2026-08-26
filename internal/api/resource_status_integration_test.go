//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/api/... -tags=integration -v -run ResourceStatus
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/api"
	"github.com/pgarchihub/pgarchimigrator/internal/progress"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// quoteIdentForResourceStatusTest mirrors the same minimal identifier
// escaping used throughout this project (see e.g. internal/ddlflow's
// quoteIdent) — needed here because job IDs in these tests contain
// hyphens (invalid in an unquoted PostgreSQL identifier), and the
// resource names shadowflow.ResourceNames derives from them can inherit
// that.
func quoteIdentForResourceStatusTest(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

const resourceStatusTestDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable"

func connectPoolForResourceStatus(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, resourceStatusTestDSN)
	if err != nil {
		t.Fatalf("could not connect (is docker compose up?): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestServerWithRealPool mirrors newTestServer (server_test.go, same
// package) but wires a REAL PostgreSQL pool instead of nil — needed
// here specifically because attachResourceStatus queries real system
// catalogs (pg_tables, pg_replication_slots, pg_publication, pg_indexes),
// unlike everything else server_test.go covers.
func newTestServerWithRealPool(t *testing.T, pool *pgxpool.Pool, store *fakeStore) (*api.Server, *testUsers) {
	t.Helper()
	srv, users := newTestServer(t, store, &fakeFlow{})
	// newTestServer's own orchestrator/auth wiring is reused as-is (this
	// test never calls StartMigration, only GET /api/migrations/{id});
	// only the Pool needs to be the real connection these tests query
	// against directly to set up/tear down test fixtures.
	srv.Pool = pool
	return srv, users
}

// TestHandleGetMigration_ResourceStatus_SHADOWTABLE_AllClean is the
// direct regression test for a real incident (see internal/api's
// attachResourceStatus doc comment): an orphaned shadow table +
// permanently mis-owned sequence sat invisible after a failed migration
// until manually found via psql. This confirms the healthy case first —
// when nothing is left behind, all three resources are reported with
// Exists: false (not omitted — see ResourceStatus's own doc comment for
// why a positive "checked, and it's clean" is shown rather than staying
// silent).
func TestHandleGetMigration_ResourceStatus_SHADOWTABLE_AllClean(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	ctx := context.Background()
	store := newFakeStore()

	job := &state.Job{
		ID: "resstatus-clean-job-1", SchemaName: "public", TableName: "resstatus_clean_table",
		Strategy: "SHADOW_TABLE", Phase: state.PhaseCompleted,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}
	// Deliberately create NO actual resources — none of shadowTable/
	// slotName/pubName exist for this job ID, matching a genuinely
	// clean, fully-cleaned-up migration.

	srv, users := newTestServerWithRealPool(t, pool, store)
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+job.ID, nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report progress.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	if len(report.ResourceStatus) != 3 {
		t.Fatalf("expected 3 resource checks (shadow table, slot, publication), got %d: %+v", len(report.ResourceStatus), report.ResourceStatus)
	}
	for _, rs := range report.ResourceStatus {
		if rs.Exists {
			t.Errorf("expected %q to be reported as NOT existing (clean), got Exists=true (%s)", rs.Name, rs.Detail)
		}
	}
}

// TestHandleGetMigration_ResourceStatus_SHADOWTABLE_DetectsLeftoverShadowTable
// is the unhealthy-case counterpart — a shadow table that's still
// present (exactly what the real incident looked like) must be reported
// with Exists: true, not silently missed.
func TestHandleGetMigration_ResourceStatus_SHADOWTABLE_DetectsLeftoverShadowTable(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	ctx := context.Background()
	store := newFakeStore()

	jobID := "resstatus-dirty-job-1"
	tableName := "resstatus_dirty_table"
	job := &state.Job{
		ID: jobID, SchemaName: "public", TableName: tableName,
		Strategy: "SHADOW_TABLE", Phase: state.PhaseFailed,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}

	shadowTable, _, _, _ := shadowflow.ResourceNames(jobID, tableName)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS `+quoteIdentForResourceStatusTest(shadowTable)); err != nil {
		t.Fatalf("could not clean up pre-existing test table: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE `+quoteIdentForResourceStatusTest(shadowTable)+` (id BIGINT)`); err != nil {
		t.Fatalf("could not create leftover shadow table fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+quoteIdentForResourceStatusTest(shadowTable))
	})

	srv, users := newTestServerWithRealPool(t, pool, store)
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+jobID, nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report progress.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	var found bool
	for _, rs := range report.ResourceStatus {
		if rs.Name == "Shadow table" {
			found = true
			if !rs.Exists {
				t.Error("expected the leftover shadow table to be reported as Exists=true")
			}
			if rs.Detail != shadowTable {
				t.Errorf("expected Detail=%q, got %q", shadowTable, rs.Detail)
			}
		}
	}
	if !found {
		t.Fatal("expected a \"Shadow table\" entry in ResourceStatus")
	}
}

// TestHandleGetMigration_ResourceStatus_NonTerminalJob_NotEnriched
// confirms attachResourceStatus's own documented scope: only terminal
// jobs get this treatment — a still-running migration's resources are
// SUPPOSED to exist, so checking/reporting them wouldn't distinguish
// healthy from concerning the way it meaningfully does once finished.
func TestHandleGetMigration_ResourceStatus_NonTerminalJob_NotEnriched(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	ctx := context.Background()
	store := newFakeStore()

	job := &state.Job{
		ID: "resstatus-running-job-1", SchemaName: "public", TableName: "resstatus_running_table",
		Strategy: "SHADOW_TABLE", Phase: state.PhaseSyncing, // not terminal
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}

	srv, users := newTestServerWithRealPool(t, pool, store)
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+job.ID, nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report progress.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if report.ResourceStatus != nil {
		t.Errorf("expected no ResourceStatus for a non-terminal job, got %+v", report.ResourceStatus)
	}
}

// TestHandleGetMigration_ResourceStatus_EXPANDBACKFILL_DetectsLeftoverIndex
// covers the other strategy attachResourceStatus checks — a lingering
// temporary backfill index (see internal/ddlflow's createBackfillIndex
// doc comment for what this index is and why a leftover one is worth
// flagging) reported by name, matched by prefix since the exact name
// isn't persisted on the job.
func TestHandleGetMigration_ResourceStatus_EXPANDBACKFILL_DetectsLeftoverIndex(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	ctx := context.Background()
	store := newFakeStore()

	tableName := "resstatus_backfill_table"
	job := &state.Job{
		ID: "resstatus-backfill-job-1", SchemaName: "public", TableName: tableName,
		Strategy: "EXPAND_BACKFILL", Phase: state.PhaseAborted,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+quoteIdentForResourceStatusTest(tableName))
	if _, err := pool.Exec(ctx, `CREATE TABLE `+quoteIdentForResourceStatusTest(tableName)+` (id BIGINT, col TIMESTAMPTZ)`); err != nil {
		t.Fatalf("could not create fixture table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+quoteIdentForResourceStatusTest(tableName))
	})

	indexName := "__pgam_backfill_idx_col_leftovertest"
	if _, err := pool.Exec(ctx,
		`CREATE INDEX `+quoteIdentForResourceStatusTest(indexName)+` ON `+quoteIdentForResourceStatusTest(tableName)+` (col) WHERE col IS NULL`,
	); err != nil {
		t.Fatalf("could not create leftover backfill index fixture: %v", err)
	}

	srv, users := newTestServerWithRealPool(t, pool, store)
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+job.ID, nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report progress.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if len(report.ResourceStatus) != 1 || !report.ResourceStatus[0].Exists {
		t.Fatalf("expected exactly one Exists=true entry for the leftover backfill index, got %+v", report.ResourceStatus)
	}
}

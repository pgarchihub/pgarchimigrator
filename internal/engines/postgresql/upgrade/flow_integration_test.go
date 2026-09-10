//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/engines/postgresql/upgrade/... -tags=integration -v -run TestFlow_Run -timeout 120s
package upgrade_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/engines/postgresql/upgrade"
)

// TestFlow_Run_EndToEnd_SmallTable is THE central proof this package's
// entire flow actually works, not just its individual pieces in
// isolation: a real table, on a real source instance, ends up as a
// faithful, verified copy on a real (separate) target instance,
// entirely through PostgreSQL's own native logical replication — the
// exact mechanism internal/engines/postgresql/upgrade/sync.go's own doc comment argues
// for over a custom pglogrepl-based engine.
func TestFlow_Run_EndToEnd_SmallTable(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_e2e_orders`)
	_, _ = targetPool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_e2e_orders`)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = sourcePool.Exec(bg, `DROP TABLE IF EXISTS upgrade_e2e_orders`)
		_, _ = targetPool.Exec(bg, `DROP TABLE IF EXISTS upgrade_e2e_orders`)
	})

	if _, err := sourcePool.Exec(ctx, `
		CREATE TABLE upgrade_e2e_orders (
			id BIGINT PRIMARY KEY,
			customer_name TEXT NOT NULL,
			amount_cents BIGINT NOT NULL
		)
	`); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := sourcePool.Exec(ctx, `
		INSERT INTO upgrade_e2e_orders (id, customer_name, amount_cents)
		SELECT g, 'customer-' || g, g * 100 FROM generate_series(1, 500) g
	`); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}

	store, err := upgrade.NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade-e2e-test.db"))
	if err != nil {
		t.Fatalf("could not create upgrade store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Tables (not Schemas) — deliberately scoped to exactly this one
	// table rather than "every table in public". This matters for real
	// test isolation: Schemas: []string{"public"} would pick up
	// whatever any OTHER integration test package currently has
	// sitting in the same shared "public" schema on this Docker
	// Compose source instance (e.g. internal/api's own
	// resource_status_integration_test.go, which uses a real pgxpool
	// against the same instance) if `go test ./...` happens to run
	// that package concurrently with this one — `go test` parallelizes
	// across packages by default even with no t.Parallel() calls
	// within either package. TablesVerified asserting exactly 1 below
	// is only a meaningful check once this test can't observe another
	// package's own tables at all, not just "usually doesn't".
	job := &upgrade.Job{
		Tables:               []upgrade.TableRef{{SchemaName: "public", TableName: "upgrade_e2e_orders"}},
		SourceConnectionRef:  sourceTestDSN,
		TargetConnectionRef:  targetTestDSN,
		SourceReplicationRef: sourceReplicationTestDSN, // see this constant's own doc comment — required, not optional, in this Docker Compose test topology
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("could not create upgrade job: %v", err)
	}

	flow := &upgrade.Flow{Store: store, ConnectionProvider: upgrade.StaticConnectionProvider{}}

	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// t.Cleanup rather than a deferred DROP SUBSCRIPTION/PUBLICATION —
	// the subscription/publication this test's own Run call creates
	// (named after job.ID, see sync.go's PublicationName/
	// SubscriptionName) need to be torn down even if the test fails
	// partway through, so a later test run against the same two
	// long-lived docker-compose instances doesn't collide with a
	// leftover subscription from this one.
	t.Cleanup(func() {
		bg := context.Background()
		_ = upgrade.DropSubscription(bg, targetPool, job.ID)
		_ = upgrade.DropPublication(bg, sourcePool, job.ID)
	})

	if err := flow.Run(runCtx, job); err != nil {
		t.Fatalf("Flow.Run failed (job phase: %s, last error: %s): %v", job.Phase, job.LastError, err)
	}

	if job.Phase != upgrade.PhaseReady {
		t.Errorf("expected the job to reach PhaseReady, got %s", job.Phase)
	}
	if job.TablesVerified != 1 {
		t.Errorf("expected exactly 1 table verified, got %d", job.TablesVerified)
	}

	// The real, behavioral proof: query the TARGET directly and confirm
	// the data genuinely arrived — not just that Flow.Run returned nil.
	var count int64
	if err := targetPool.QueryRow(ctx, `SELECT count(*) FROM upgrade_e2e_orders`).Scan(&count); err != nil {
		t.Fatalf("could not query target table: %v", err)
	}
	if count != 500 {
		t.Errorf("expected 500 rows to have been replicated to the target, got %d", count)
	}

	var sampleName string
	if err := targetPool.QueryRow(ctx, `SELECT customer_name FROM upgrade_e2e_orders WHERE id = 250`).Scan(&sampleName); err != nil {
		t.Fatalf("could not query a specific replicated row: %v", err)
	}
	if sampleName != "customer-250" {
		t.Errorf("expected customer_name 'customer-250' for id=250, got %q", sampleName)
	}

	// TestFlow_Run_EndToEnd_SmallTable's own direct regression test for
	// a real bug found during manual GUI testing: Table.RowsSynced
	// stayed at 0 for a job that had genuinely and successfully synced
	// 500 rows — Store.UpdateTableRowsSynced was defined but never
	// actually called anywhere in Flow.sync. See updateRowCounts's own
	// doc comment for the fix.
	tables, err := store.ListTables(ctx, job.ID)
	if err != nil {
		t.Fatalf("could not list tables for job %s: %v", job.ID, err)
	}
	if len(tables) != 1 {
		t.Fatalf("expected exactly 1 table record, got %d", len(tables))
	}
	if tables[0].RowsSynced != 500 {
		t.Errorf("expected RowsSynced=500 (the real bug: this stayed at 0), got %d", tables[0].RowsSynced)
	}
}

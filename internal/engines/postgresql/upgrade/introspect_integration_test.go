//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/engines/postgresql/upgrade/... -tags=integration -v -run TestIntrospect -timeout 60s
//	go test ./internal/engines/postgresql/upgrade/... -tags=integration -v -run TestCreateTableOnTarget -timeout 60s
package upgrade_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/engines/postgresql/upgrade"
)

// sourceTestDSN/targetTestDSN point at deploy/docker-compose.dev.yml's
// own pg-logical (source) and pg-upgrade-target (target) services —
// see that file's own comments for why internal/engines/postgresql/upgrade needs a
// genuinely SEPARATE second instance, unlike every other integration
// test in this repo (internal/engines/postgresql/shadowflow's own), which syncs within one
// instance.
const (
	sourceTestDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable"
	targetTestDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55434/pgarchimigrator_test?sslmode=disable"
	// sourceReplicationTestDSN is sourceTestDSN's own network-topology
	// counterpart for CREATE SUBSCRIPTION specifically — see
	// upgrade.Job.SourceReplicationRef's own doc comment for the real,
	// user-reported bug this distinction exists to fix: CREATE
	// SUBSCRIPTION is executed by pg-upgrade-target's OWN PostgreSQL
	// server, not by this test process, so it needs "pg-logical" (the
	// Docker Compose network's own service hostname, reachable from
	// WITHIN pg-upgrade-target's container) rather than "localhost"
	// (which, from inside that container, refers to the container
	// itself — sourceTestDSN's host-mapped port is only reachable from
	// THIS test process, never from another container). Every test in
	// this file that calls CreateSubscription (directly, or indirectly
	// via Flow.Run) must set Job.SourceReplicationRef to this constant,
	// not leave it empty (which would silently fall back to
	// sourceTestDSN and fail exactly the way the real bug report did).
	sourceReplicationTestDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@pg-logical:5432/pgarchimigrator_test?sslmode=disable"
)

func connectTestPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("could not connect to %s (is docker compose up?): %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIntrospect_DiscoversTablesAcrossSchemas(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP SCHEMA IF EXISTS upgrade_introspect_test CASCADE`)
	if _, err := sourcePool.Exec(ctx, `CREATE SCHEMA upgrade_introspect_test`); err != nil {
		t.Fatalf("could not create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = sourcePool.Exec(context.Background(), `DROP SCHEMA IF EXISTS upgrade_introspect_test CASCADE`)
	})

	if _, err := sourcePool.Exec(ctx, `CREATE TABLE upgrade_introspect_test.orders (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if _, err := sourcePool.Exec(ctx, `CREATE TABLE upgrade_introspect_test.customers (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}

	job := &upgrade.Job{Schemas: []string{"upgrade_introspect_test"}}
	refs, err := upgrade.Introspect(ctx, sourcePool, job)
	if err != nil {
		t.Fatalf("Introspect failed: %v", err)
	}

	found := make(map[string]bool)
	for _, r := range refs {
		found[r.String()] = true
	}
	if !found["upgrade_introspect_test.orders"] {
		t.Error("expected orders to be discovered")
	}
	if !found["upgrade_introspect_test.customers"] {
		t.Error("expected customers to be discovered")
	}
}

func TestCreateTableOnTarget_RecreatesColumnsAndPrimaryKey(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_ctot_source`)
	_, _ = targetPool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_ctot_source`)
	t.Cleanup(func() {
		_, _ = sourcePool.Exec(context.Background(), `DROP TABLE IF EXISTS upgrade_ctot_source`)
		_, _ = targetPool.Exec(context.Background(), `DROP TABLE IF EXISTS upgrade_ctot_source`)
	})

	if _, err := sourcePool.Exec(ctx, `
		CREATE TABLE upgrade_ctot_source (
			id BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			note TEXT
		)
	`); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}

	ref := upgrade.TableRef{SchemaName: "public", TableName: "upgrade_ctot_source"}
	if err := upgrade.CreateTableOnTarget(ctx, sourcePool, targetPool, ref); err != nil {
		t.Fatalf("CreateTableOnTarget failed: %v", err)
	}

	// Prove the recreated table genuinely accepts the same shape of
	// data and enforces the same constraints — a real behavioral check,
	// not just "some table now exists."
	if _, err := targetPool.Exec(ctx, `INSERT INTO upgrade_ctot_source (id, name) VALUES (1, 'test')`); err != nil {
		t.Errorf("expected the recreated table to accept a valid insert, got: %v", err)
	}
	if _, err := targetPool.Exec(ctx, `INSERT INTO upgrade_ctot_source (id, name) VALUES (1, 'duplicate')`); err == nil {
		t.Error("expected the recreated table's PRIMARY KEY to reject a duplicate id")
	}
	if _, err := targetPool.Exec(ctx, `INSERT INTO upgrade_ctot_source (id, name) VALUES (2, NULL)`); err == nil {
		t.Error("expected the recreated table's NOT NULL on name to reject a null value")
	}
}

func TestCreateTableOnTarget_NoColumns_Fails(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	ref := upgrade.TableRef{SchemaName: "public", TableName: fmt.Sprintf("upgrade_nonexistent_%d", 12345)}
	err := upgrade.CreateTableOnTarget(ctx, sourcePool, targetPool, ref)
	if err == nil {
		t.Fatal("expected an error introspecting a table that does not exist on the source")
	}
}

// TestCreateTableOnTarget_TableAlreadyExistsOnTarget_GivesActionableError
// is the direct regression test for a real bug report from manual
// testing: a RETRY against a target that already has this table (left
// behind by an earlier attempt that got past schema creation before
// failing at a later step) must get a clear, actionable explanation —
// not PostgreSQL's own bare "already exists" text unexplained.
func TestCreateTableOnTarget_TableAlreadyExistsOnTarget_GivesActionableError(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_duplicate_test`)
	_, _ = targetPool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_duplicate_test`)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = sourcePool.Exec(bg, `DROP TABLE IF EXISTS upgrade_duplicate_test`)
		_, _ = targetPool.Exec(bg, `DROP TABLE IF EXISTS upgrade_duplicate_test`)
	})

	if _, err := sourcePool.Exec(ctx, `CREATE TABLE upgrade_duplicate_test (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}

	ref := upgrade.TableRef{SchemaName: "public", TableName: "upgrade_duplicate_test"}
	// First call: succeeds normally, simulating an earlier attempt that
	// got this far before failing at a LATER step.
	if err := upgrade.CreateTableOnTarget(ctx, sourcePool, targetPool, ref); err != nil {
		t.Fatalf("expected the first CreateTableOnTarget call to succeed, got: %v", err)
	}

	// Second call (the "retry"): must fail with a message that actually
	// explains WHY and WHAT TO DO, not a bare PostgreSQL error.
	err := upgrade.CreateTableOnTarget(ctx, sourcePool, targetPool, ref)
	if err == nil {
		t.Fatal("expected the second call (against a target that already has this table) to fail")
	}
	if !strings.Contains(err.Error(), "already exists on the target") {
		t.Errorf("expected an actionable 'already exists on the target' explanation, got: %v", err)
	}
	if !strings.Contains(err.Error(), "DROP TABLE") {
		t.Errorf("expected the error to suggest the DROP TABLE remedy, got: %v", err)
	}
}

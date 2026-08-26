//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/db/... -tags=integration -v
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

const (
	// pg-logical: wal_level=logical, has both orders (with PK) and legacy_no_pk (without PK) tables.
	logicalDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable"
	// pg-legacy: wal_level=replica — for the US-05 scenario.
	legacyDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55433/pgarchimigrator_test?sslmode=disable"
)

func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("could not connect (is docker compose up?): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCheckShadowTablePreconditions_LogicalInstance_TableWithPK_Passes(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS preflight_with_pk_test`); err != nil {
		t.Fatalf("could not clean up old table: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE preflight_with_pk_test (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS preflight_with_pk_test`) })

	pf := db.NewPgxPreflighter(pool)
	result, err := pf.CheckShadowTablePreconditions(ctx, "public", "preflight_with_pk_test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := result.Validate(); err != nil {
		t.Errorf("preflight should have passed for a table with a PK, got: %v", err)
	}
	if !result.WALLevelLogical {
		t.Error("expected wal_level=logical")
	}
	if !result.TargetHasPrimaryKey {
		t.Error("expected the test table to have a PRIMARY KEY")
	}
	if result.PostgresVersion < 12 {
		t.Errorf("expected PostgreSQL >= 12, got %d", result.PostgresVersion)
	}
}

// US-05: verifies that preflight rejects a table without a PK with a
// meaningful error. This also proves the "relreplident='d' but no PK"
// distinction is handled correctly (see the comment in
// pgx_preflighter.go's checkPrimaryKey).
func TestCheckShadowTablePreconditions_TableWithoutPK_Fails(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS preflight_no_pk_test`); err != nil {
		t.Fatalf("could not clean up old table: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE preflight_no_pk_test (note TEXT)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS preflight_no_pk_test`) })

	pf := db.NewPgxPreflighter(pool)
	result, err := pf.CheckShadowTablePreconditions(ctx, "public", "preflight_no_pk_test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TargetHasPrimaryKey {
		t.Error("the test table should have been detected as having NO PRIMARY KEY")
	}
	if err := result.Validate(); err == nil {
		t.Error("expected preflight to return an error without a PK")
	}
}

// US-05: verifies that preflight fails clearly on an instance with
// wal_level=replica.
func TestCheckShadowTablePreconditions_LegacyInstance_WrongWALLevel_Fails(t *testing.T) {
	pool := connect(t, legacyDSN)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS preflight_probe (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY
		)
	`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS preflight_probe`)
	})

	pf := db.NewPgxPreflighter(pool)
	result, err := pf.CheckShadowTablePreconditions(context.Background(), "public", "preflight_probe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WALLevelLogical {
		t.Error("expected WALLevelLogical=false on an instance with wal_level=replica")
	}
	if err := result.Validate(); err == nil {
		t.Error("expected preflight to return an error with wal_level=replica")
	}
}

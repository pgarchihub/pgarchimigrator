//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

func TestFetchTableStats_TableWithPK(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS table_stats_test`); err != nil {
		t.Fatalf("could not clean up old table: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE table_stats_test (id BIGINT PRIMARY KEY, name TEXT)
	`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS table_stats_test`) })

	if _, err := pool.Exec(ctx, `
		INSERT INTO table_stats_test (id, name) SELECT g, 'row' || g FROM generate_series(1, 100) g
	`); err != nil {
		t.Fatalf("could not seed test table: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE table_stats_test`); err != nil {
		t.Fatalf("could not analyze test table: %v", err)
	}

	stats, err := db.FetchTableStats(ctx, pool, "public", "table_stats_test")
	if err != nil {
		t.Fatalf("FetchTableStats failed: %v", err)
	}
	if !stats.HasPrimaryKey {
		t.Error("expected HasPrimaryKey=true")
	}
	if stats.IsPartitioned {
		t.Error("expected IsPartitioned=false")
	}
	if stats.EstimatedRowCount < 90 || stats.EstimatedRowCount > 110 {
		t.Errorf("expected EstimatedRowCount close to 100 (planner estimate after ANALYZE), got %d", stats.EstimatedRowCount)
	}
}

// TestFetchTableStats_TableWithoutPK creates its own PK-less table rather
// than relying on the "legacy_no_pk" fixture from the setup guide's Section
// 3.2 — that table only exists if the pg-logical volume hasn't been reset
// since it was seeded (e.g. it's gone after a `docker compose down -v`).
func TestFetchTableStats_TableWithoutPK(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS table_stats_no_pk_test`); err != nil {
		t.Fatalf("could not clean up old table: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE table_stats_no_pk_test (note TEXT)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS table_stats_no_pk_test`) })

	stats, err := db.FetchTableStats(ctx, pool, "public", "table_stats_no_pk_test")
	if err != nil {
		t.Fatalf("FetchTableStats failed: %v", err)
	}
	if stats.HasPrimaryKey {
		t.Error("expected HasPrimaryKey=false")
	}
}

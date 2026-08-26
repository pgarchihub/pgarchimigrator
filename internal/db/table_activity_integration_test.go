//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/db/... -tags=integration -v -run TableActivity
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

func TestFetchTableActivity_NoActiveQueries_ReturnsZero(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()
	tableName := "table_activity_test_quiet"

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tableName)
	if _, err := pool.Exec(ctx, `CREATE TABLE `+tableName+` (id BIGINT)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+tableName) })

	activity, err := db.FetchTableActivity(ctx, pool, "public", tableName)
	if err != nil {
		t.Fatalf("FetchTableActivity failed: %v", err)
	}
	if activity.ActiveQueries != 0 {
		t.Errorf("expected 0 active queries against a table nothing is touching, got %d", activity.ActiveQueries)
	}
	if activity.MaxDurationSeconds != 0 {
		t.Errorf("expected MaxDurationSeconds=0 when there are no active queries, got %v", activity.MaxDurationSeconds)
	}
}

// TestFetchTableActivity_DetectsAGenuinelyActiveQuery is the direct
// end-to-end proof this works against real PostgreSQL: starts a
// deliberately slow query against the table on a separate, concurrent
// connection, then confirms FetchTableActivity (called from the main
// test connection, while that query is still running) detects it.
func TestFetchTableActivity_DetectsAGenuinelyActiveQuery(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()
	tableName := "table_activity_test_busy"

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tableName)
	if _, err := pool.Exec(ctx, `CREATE TABLE `+tableName+` (id BIGINT)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+tableName+` VALUES (1)`); err != nil {
		t.Fatalf("could not seed test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+tableName) })

	// A genuinely slow query against the table, on its own connection,
	// running in the background — SELECT ... pg_sleep(...) holds an
	// AccessShareLock on the table for the whole sleep duration, which
	// is exactly what FetchTableActivity's pg_locks join looks for.
	go func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_sleep(2) FROM `+tableName)
	}()

	// Give the background query a moment to actually start and acquire
	// its lock before sampling — this isn't a race the production code
	// needs to handle (a real migration's write traffic isn't
	// synchronized with when someone happens to poll), just this test's
	// own setup needing the background goroutine to have gotten going.
	time.Sleep(300 * time.Millisecond)

	activity, err := db.FetchTableActivity(ctx, pool, "public", tableName)
	if err != nil {
		t.Fatalf("FetchTableActivity failed: %v", err)
	}
	if activity.ActiveQueries < 1 {
		t.Errorf("expected at least 1 active query while the background pg_sleep is running, got %d", activity.ActiveQueries)
	}
	if activity.MaxDurationSeconds <= 0 {
		t.Errorf("expected a positive MaxDurationSeconds while a query has been running for a noticeable amount of time, got %v", activity.MaxDurationSeconds)
	}
}

// TestFetchTableActivity_DoesNotCountActivityOnADifferentTable confirms
// the schema/table filter is precise — a query against some OTHER table
// must not be counted, proving this isn't accidentally matching every
// active session server-wide.
func TestFetchTableActivity_DoesNotCountActivityOnADifferentTable(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()
	busyTable := "table_activity_test_other_busy"
	quietTable := "table_activity_test_other_quiet"

	for _, tbl := range []string{busyTable, quietTable} {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl)
		if _, err := pool.Exec(ctx, `CREATE TABLE `+tbl+` (id BIGINT)`); err != nil {
			t.Fatalf("could not create test table %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+busyTable)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+quietTable)
	})

	go func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_sleep(2) FROM `+busyTable)
	}()
	time.Sleep(300 * time.Millisecond)

	activity, err := db.FetchTableActivity(ctx, pool, "public", quietTable)
	if err != nil {
		t.Fatalf("FetchTableActivity failed: %v", err)
	}
	if activity.ActiveQueries != 0 {
		t.Errorf("expected 0 active queries against the QUIET table even while the OTHER table is busy, got %d", activity.ActiveQueries)
	}
}

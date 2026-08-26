//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/shadowflow/... -tags=integration -v
package shadowflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
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

// createSwapTestTables creates two tables — oldTable (as if it were the
// live table) and newTable (as if it were the fully-synced shadow table) —
// each with a distinguishable single row, so the test can verify which
// table ends up under which name after the swap.
func createSwapTestTables(t *testing.T, pool *pgxpool.Pool, oldTable, newTable, tempName string) {
	t.Helper()
	ctx := context.Background()

	for _, name := range []string{oldTable, newTable, tempName} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
			t.Fatalf("could not clean up old table %s: %v", name, err)
		}
	}

	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, source TEXT)`, oldTable)); err != nil {
		t.Fatalf("could not create old table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 'old')`, oldTable)); err != nil {
		t.Fatalf("could not insert into old table: %v", err)
	}

	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, source TEXT)`, newTable)); err != nil {
		t.Fatalf("could not create new (shadow) table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 'new')`, newTable)); err != nil {
		t.Fatalf("could not insert into new table: %v", err)
	}

	t.Cleanup(func() {
		for _, name := range []string{oldTable, newTable, tempName} {
			_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name))
		}
	})
}

func readSource(t *testing.T, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var source string
	err := pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT source FROM %s WHERE id = 1`, table)).Scan(&source)
	if err != nil {
		t.Fatalf("could not read from %s: %v", table, err)
	}
	return source
}

// TestSwap_Success_NoLock verifies the happy path: with no competing lock,
// a single attempt should succeed, and after the swap the "old" name should
// contain what used to be the "new" (shadow) table's data, while the
// original old-table content should now live under tempName.
func TestSwap_Success_NoLock(t *testing.T) {
	pool := connectPool(t)
	oldTable, newTable, tempName := "swap_old_1", "swap_new_1", "swap_temp_1"
	createSwapTestTables(t, pool, oldTable, newTable, tempName)

	executor := shadowflow.NewSwapExecutor(pool)
	cfg := shadowflow.DefaultSwapConfig()

	if err := executor.Swap(context.Background(), "public", oldTable, newTable, tempName, cfg); err != nil {
		t.Fatalf("Swap failed: %v", err)
	}

	if got := readSource(t, pool, oldTable); got != "new" {
		t.Errorf("expected the 'old' name to now hold the new table's data ('new'), got %q", got)
	}
	if got := readSource(t, pool, tempName); got != "old" {
		t.Errorf("expected tempName to hold the original old table's data ('old'), got %q", got)
	}
}

// TestSwap_RetriesAndSucceeds_WhenLockReleasedBeforeMaxRetries deliberately
// holds a lock on oldTable in a separate transaction, starts Swap in a
// goroutine (which will hit lock_timeout on its first attempt(s)), then
// releases the lock before MaxRetries is exhausted — Swap should recover
// and eventually succeed on a later attempt.
func TestSwap_RetriesAndSucceeds_WhenLockReleasedBeforeMaxRetries(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	oldTable, newTable, tempName := "swap_old_2", "swap_new_2", "swap_temp_2"
	createSwapTestTables(t, pool, oldTable, newTable, tempName)

	blockingConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("could not acquire connection: %v", err)
	}
	defer blockingConn.Release()

	tx, err := blockingConn.Begin(ctx)
	if err != nil {
		t.Fatalf("could not begin blocking transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET source = 'locked' WHERE id = 1`, oldTable)); err != nil {
		t.Fatalf("could not lock old table: %v", err)
	}

	executor := shadowflow.NewSwapExecutor(pool)
	cfg := shadowflow.SwapConfig{
		LockTimeout: 200 * time.Millisecond,
		MaxRetries:  5,
		BackoffBase: 150 * time.Millisecond,
	}

	swapDone := make(chan error, 1)
	go func() {
		swapDone <- executor.Swap(context.Background(), "public", oldTable, newTable, tempName, cfg)
	}()

	// Hold the lock long enough for at least one retry to hit lock_timeout,
	// then release it well before MaxRetries would be exhausted.
	time.Sleep(500 * time.Millisecond)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("could not release the lock: %v", err)
	}

	select {
	case err := <-swapDone:
		if err != nil {
			t.Fatalf("Swap should have eventually succeeded after the lock was released, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Swap to complete")
	}

	if got := readSource(t, pool, oldTable); got != "new" {
		t.Errorf("expected the swap to have completed, got source=%q under the old name", got)
	}
}

// TestSwap_FailsAfterMaxRetries_WhenLockNeverReleases verifies that when
// the blocking lock is held for the entire test, Swap exhausts MaxRetries
// and returns an error, and — critically — the original tables are left
// completely untouched (no partial rename), since every attempt runs in
// its own transaction.
func TestSwap_FailsAfterMaxRetries_WhenLockNeverReleases(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	oldTable, newTable, tempName := "swap_old_3", "swap_new_3", "swap_temp_3"
	createSwapTestTables(t, pool, oldTable, newTable, tempName)

	blockingConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("could not acquire connection: %v", err)
	}
	defer blockingConn.Release()

	tx, err := blockingConn.Begin(ctx)
	if err != nil {
		t.Fatalf("could not begin blocking transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET source = 'locked' WHERE id = 1`, oldTable)); err != nil {
		t.Fatalf("could not lock old table: %v", err)
	}

	executor := shadowflow.NewSwapExecutor(pool)
	cfg := shadowflow.SwapConfig{
		LockTimeout: 100 * time.Millisecond,
		MaxRetries:  2,
		BackoffBase: 50 * time.Millisecond,
	}

	err = executor.Swap(context.Background(), "public", oldTable, newTable, tempName, cfg)
	if err == nil {
		t.Fatal("expected Swap to fail while the lock is held for the entire duration")
	}

	// Verify the original tables were left untouched: oldTable still has
	// 'old', newTable still has 'new', tempName was never created. This can
	// be read safely even while the blocking transaction still holds its
	// lock: thanks to MVCC, a plain SELECT from another connection sees the
	// last committed value, not the blocking transaction's uncommitted write.
	if got := readSource(t, pool, oldTable); got != "old" {
		t.Errorf("old table should be untouched after a failed swap, got source=%q", got)
	}
	if got := readSource(t, pool, newTable); got != "new" {
		t.Errorf("new table should be untouched after a failed swap, got source=%q", got)
	}

	var tempExists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tempName,
	).Scan(&tempExists); err != nil {
		t.Fatalf("could not check for tempName's existence: %v", err)
	}
	if tempExists {
		t.Error("tempName should not exist after a failed swap (no partial rename should have occurred)")
	}
}

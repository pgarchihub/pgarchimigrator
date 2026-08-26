//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/ddlflow/... -tags=integration -v -run LockTimeout

// This file is deliberately `package ddlflow` (internal/white-box), not
// `package ddlflow_test` like ddlflow_integration_test.go — it needs
// direct access to the unexported execDDLWithLockTimeout,
// execOnceWithLockTimeout, and isLockTimeoutError.
package ddlflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const lockTimeoutTestDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable"

func lockTimeoutTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, lockTimeoutTestDSN)
	if err != nil {
		t.Fatalf("could not connect (is docker compose up?): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// lockTableInBackground begins a transaction, locks tableName with a
// `SELECT ... FOR UPDATE` (any row lock conflicts with the ACCESS
// EXCLUSIVE an ALTER TABLE needs), and holds it until releaseAfter
// elapses. Returns a channel closed once the lock is confirmed held, so
// callers don't race ahead of the blocker actually acquiring it.
func lockTableInBackground(t *testing.T, pool *pgxpool.Pool, tableName string, holdFor time.Duration) <-chan struct{} {
	t.Helper()
	ready := make(chan struct{})
	go func() {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Errorf("blocker: failed to begin: %v", err)
			close(ready)
			return
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), "SELECT * FROM "+tableName+" FOR UPDATE"); err != nil {
			t.Errorf("blocker: failed to lock: %v", err)
			close(ready)
			return
		}
		close(ready)
		time.Sleep(holdFor)
	}()
	return ready
}

func makeLockTimeoutTestTable(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+name)
	if _, err := pool.Exec(ctx, "CREATE TABLE "+name+" (id INT)"); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO "+name+" (id) VALUES (1)"); err != nil {
		t.Fatalf("could not seed a row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+name) })
}

func TestExecDDLWithLockTimeout_SucceedsImmediatelyWithNoContention(t *testing.T) {
	pool := lockTimeoutTestPool(t)
	ctx := context.Background()
	tableName := "lock_timeout_test_no_contention"
	makeLockTimeoutTestTable(t, pool, tableName)

	start := time.Now()
	if err := execDDLWithLockTimeout(ctx, pool, "ALTER TABLE "+tableName+" ADD COLUMN flag BOOLEAN"); err != nil {
		t.Fatalf("expected success with no contention, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("expected this to complete quickly with no contention, took %s", elapsed)
	}
}

// TestExecDDLWithLockTimeout_RetriesAndSucceedsOnceTheBlockerReleases is
// the direct regression test for the real bug a 10M-row load test found:
// a concurrent transaction holding ANY lock on the table (even one that
// wouldn't conflict with ordinary application reads/writes) blocks an
// ALTER TABLE from acquiring ACCESS EXCLUSIVE. This proves
// execDDLWithLockTimeout retries rather than hanging indefinitely, and
// succeeds once the blocker releases.
func TestExecDDLWithLockTimeout_RetriesAndSucceedsOnceTheBlockerReleases(t *testing.T) {
	pool := lockTimeoutTestPool(t)
	ctx := context.Background()
	tableName := "lock_timeout_test_retry_success"
	makeLockTimeoutTestTable(t, pool, tableName)

	// Held for ~1.5s — well within the retry budget (3s lock_timeout per
	// attempt, up to 5 attempts) — so the DDL should succeed on a retry
	// once this releases, not time out entirely.
	ready := lockTableInBackground(t, pool, tableName, 1500*time.Millisecond)
	<-ready

	start := time.Now()
	err := execDDLWithLockTimeout(ctx, pool, "ALTER TABLE "+tableName+" ADD COLUMN flag BOOLEAN")
	if err != nil {
		t.Fatalf("expected the DDL to eventually succeed after retrying, got: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 1*time.Second {
		t.Errorf("expected this to take at least ~1.5s (waiting for the blocker to release) — took %s, did it not actually contend?", elapsed)
	}
	t.Logf("succeeded after %s (blocker held the lock for ~1.5s)", elapsed)
}

func TestIsLockTimeoutError_RecognizesSQLSTATE55P03(t *testing.T) {
	pool := lockTimeoutTestPool(t)
	ctx := context.Background()
	tableName := "lock_timeout_test_error_recognition"
	makeLockTimeoutTestTable(t, pool, tableName)

	// Held well past this single attempt's 3s lock_timeout, so
	// execOnceWithLockTimeout (the single-attempt primitive, not the
	// retrying wrapper) is guaranteed to actually hit SQLSTATE 55P03.
	ready := lockTableInBackground(t, pool, tableName, 5*time.Second)
	<-ready

	err := execOnceWithLockTimeout(ctx, pool, "ALTER TABLE "+tableName+" ADD COLUMN flag BOOLEAN")
	if err == nil {
		t.Fatal("expected a lock_timeout error while the blocker holds its lock, got success")
	}
	if !isLockTimeoutError(err) {
		t.Errorf("expected isLockTimeoutError to recognize this failure (SQLSTATE 55P03), got: %v", err)
	}
}

// TestExecDDLWithLockTimeout_SurfacesNonLockErrorsImmediately proves the
// function doesn't blindly retry EVERY failure — only ones it can
// identify as a lock_timeout. A genuine error (bad SQL, in this case)
// must come back on the very first attempt, not after burning through
// several retries and ~7.5s of backoff for no reason.
func TestExecDDLWithLockTimeout_SurfacesNonLockErrorsImmediately(t *testing.T) {
	pool := lockTimeoutTestPool(t)
	ctx := context.Background()

	start := time.Now()
	err := execDDLWithLockTimeout(ctx, pool, "ALTER TABLE this_table_does_not_exist_anywhere ADD COLUMN flag BOOLEAN")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error for a nonexistent table")
	}
	if isLockTimeoutError(err) {
		t.Error("this should be a plain 'table does not exist' error, not classified as a lock_timeout")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected an immediate failure with no retries, took %s (looks like it retried when it shouldn't have)", elapsed)
	}
}

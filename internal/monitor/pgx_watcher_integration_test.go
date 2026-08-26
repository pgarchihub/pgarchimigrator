//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/monitor/... -tags=integration -v
package monitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/monitor"
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

// TestCheckLockWait_NoLocks_ReturnsFalse verifies CheckLockWait returns
// false under normal conditions (no pending locks).
func TestCheckLockWait_NoLocks_ReturnsFalse(t *testing.T) {
	pool := connectPool(t)
	w := monitor.NewPgxWatcher(pool, monitor.DefaultThresholds())

	waiting, err := w.CheckLockWait(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waiting {
		t.Error("expected no pending locks")
	}
}

// TestCheckLockWait_WithBlockingTransaction_ReturnsTrue deliberately locks a
// row and then tries to update the same row from another connection to
// create a real "pending lock" scenario (Architecture Doc Section 3.3 "Lock
// Detector" — the same scenario that underlies the swap.go lock_timeout risk).
func TestCheckLockWait_WithBlockingTransaction_ReturnsTrue(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS lock_test_probe (id BIGINT PRIMARY KEY, val TEXT)
	`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS lock_test_probe`)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO lock_test_probe (id, val) VALUES (1, 'a')
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatalf("could not insert test data: %v", err)
	}

	// Connection 1: lock the row and keep the transaction open.
	blockingConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("could not acquire connection: %v", err)
	}
	defer blockingConn.Release()

	tx, err := blockingConn.Begin(ctx)
	if err != nil {
		t.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `UPDATE lock_test_probe SET val = 'locked' WHERE id = 1`); err != nil {
		t.Fatalf("could not lock row: %v", err)
	}

	// Connection 2: try to update the same row, which will block (in the background).
	blockedDone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(context.Background(), `UPDATE lock_test_probe SET val = 'blocked' WHERE id = 1`)
		blockedDone <- err
	}()

	// Short wait so the lock actually gets established (for the goroutine's
	// UPDATE to be sent and for PostgreSQL to start blocking it).
	time.Sleep(300 * time.Millisecond)

	w := monitor.NewPgxWatcher(pool, monitor.DefaultThresholds())
	waiting, err := w.CheckLockWait(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !waiting {
		t.Error("expected a pending lock to be detected")
	}

	// Cleanup: roll back the transaction and wait for the blocked goroutine to be released.
	tx.Rollback(ctx)
	<-blockedDone
}

// TestPgxWatcher_Start_ProducesSignals verifies that the polling loop
// actually produces signals against a real connection pool.
func TestPgxWatcher_Start_ProducesSignals(t *testing.T) {
	pool := connectPool(t)
	w := monitor.NewPgxWatcher(pool, monitor.DefaultThresholds())
	w.PollInterval = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	ch, err := w.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case sig, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if sig != monitor.SignalProceed && sig != monitor.SignalSlowDown && sig != monitor.SignalPause {
			t.Errorf("invalid signal: %s", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out: no signal received")
	}
}

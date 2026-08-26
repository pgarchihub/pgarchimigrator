//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/db/... -tags=integration -v -run ReplicationLag
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

func connectForLagTest(t *testing.T) *pgxpool.Pool {
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

func TestFetchReplicationLag_NoSuchSlot_ReturnsNotFoundNotError(t *testing.T) {
	pool := connectForLagTest(t)
	ctx := context.Background()

	lag, found, err := db.FetchReplicationLag(ctx, pool, "this_slot_definitely_does_not_exist")
	if err != nil {
		t.Fatalf("expected no error for a nonexistent slot (this is a normal state, not a failure), got: %v", err)
	}
	if found {
		t.Error("expected found=false for a nonexistent slot")
	}
	if lag != (db.ReplicationLag{}) {
		t.Errorf("expected a zero-value ReplicationLag when not found, got %+v", lag)
	}
}

// TestFetchReplicationLag_ExistingSlot_ReturnsFound creates a real
// logical replication slot directly via SQL (pg_create_logical_replication_slot
// — a plain function call, unlike shadowflow.CreateReplicationSlotAndGetStartLSN
// which needs a special replication-protocol connection this package has
// no reason to depend on) and confirms FetchReplicationLag finds it.
//
// Doesn't assert a specific LagBytes value — a freshly created slot that
// nothing has consumed from yet has a NULL confirmed_flush_lsn (see
// FetchReplicationLag's own doc comment on why that maps to 0, not an
// error), and forcing genuinely non-zero, predictable lag would need a
// real consumer connection this test doesn't need for its actual
// purpose: proving the slot lookup itself works end-to-end against real
// PostgreSQL, not exercising every possible lag value.
func TestFetchReplicationLag_ExistingSlot_ReturnsFound(t *testing.T) {
	pool := connectForLagTest(t)
	ctx := context.Background()
	slotName := "pgam_test_lag_slot"

	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slotName) // clean slate, ignore error if it didn't exist
	if _, err := pool.Exec(ctx, `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, slotName); err != nil {
		t.Fatalf("could not create test replication slot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_drop_replication_slot($1)`, slotName)
	})

	lag, found, err := db.FetchReplicationLag(ctx, pool, slotName)
	if err != nil {
		t.Fatalf("FetchReplicationLag failed: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a slot that genuinely exists")
	}
	if lag.LagBytes < 0 {
		t.Errorf("expected a non-negative LagBytes, got %d", lag.LagBytes)
	}
}

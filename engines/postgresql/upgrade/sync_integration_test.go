//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./engines/postgresql/upgrade/... -tags=integration -v -run TestSyncProgress -timeout 60s
package upgrade_test

import (
	"context"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/upgrade"
)

// TestSyncProgress_ReadsSrsubstateWithoutScanError is a direct,
// isolated regression test for a real bug found during manual GUI
// testing: pg_subscription_rel.srsubstate is PostgreSQL's own internal
// "char" type (OID 18, distinct from text/varchar), which pgx cannot
// decode directly into a Go string over the binary wire protocol —
// scanning it raised "cannot scan char (OID 18) in binary format into
// *string". querySubstates now casts it to ::text in the query itself
// (see sync.go's own SQL), fixing this regardless of driver-specific
// type-decoding quirks. TestFlow_Run_EndToEnd_SmallTable also exercises
// this code path as part of its own broader flow, but a failure there
// doesn't point at WHICH step broke — this test isolates SyncProgress/
// AllTablesReady specifically, so a future regression here fails fast
// and unambiguously.
func TestSyncProgress_ReadsSrsubstateWithoutScanError(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_syncprogress_test`)
	_, _ = targetPool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_syncprogress_test`)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = sourcePool.Exec(bg, `DROP TABLE IF EXISTS upgrade_syncprogress_test`)
		_, _ = targetPool.Exec(bg, `DROP TABLE IF EXISTS upgrade_syncprogress_test`)
	})

	if _, err := sourcePool.Exec(ctx, `CREATE TABLE upgrade_syncprogress_test (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}

	ref := upgrade.TableRef{SchemaName: "public", TableName: "upgrade_syncprogress_test"}
	if err := upgrade.CreateTableOnTarget(ctx, sourcePool, targetPool, ref); err != nil {
		t.Fatalf("CreateTableOnTarget failed: %v", err)
	}

	jobID := "synctest_" + time.Now().Format("150405")
	t.Cleanup(func() {
		bg := context.Background()
		_ = upgrade.DropSubscription(bg, targetPool, jobID)
		_ = upgrade.DropPublication(bg, sourcePool, jobID)
	})

	if err := upgrade.CreatePublication(ctx, sourcePool, jobID, []upgrade.TableRef{ref}); err != nil {
		t.Fatalf("CreatePublication failed: %v", err)
	}
	// sourceReplicationTestDSN, not sourceTestDSN — CREATE SUBSCRIPTION
	// is executed by pg-upgrade-target's OWN server, which needs the
	// Docker Compose network's own service hostname, not the
	// host-mapped port this test process itself uses. See that
	// constant's own doc comment for the full reasoning (and the real
	// bug report this distinction exists to prevent regressing on).
	if err := upgrade.CreateSubscription(ctx, targetPool, jobID, sourceReplicationTestDSN); err != nil {
		t.Fatalf("CreateSubscription failed: %v", err)
	}

	// The real proof: this must not return a scan error, regardless of
	// what srsubstate value PostgreSQL currently reports.
	states, err := upgrade.SyncProgress(ctx, targetPool, jobID)
	if err != nil {
		t.Fatalf("SyncProgress failed (this is the exact bug being regression-tested): %v", err)
	}
	if len(states) != 1 {
		t.Errorf("expected exactly 1 table's sync state, got %d", len(states))
	}

	if _, err := upgrade.AllTablesReady(ctx, targetPool, jobID, []upgrade.TableRef{ref}); err != nil {
		t.Fatalf("AllTablesReady failed (this is the exact bug being regression-tested): %v", err)
	}
}

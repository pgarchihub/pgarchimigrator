//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/db/... -tags=integration -v -run CheckpointPressure
package db_test

import (
	"context"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

// TestFetchCheckpointPressure_UsesTheRightViewForTheRunningVersion
// verifies db.FetchCheckpointPressure's version-aware query selection
// (pg_stat_bgwriter pre-17, pg_stat_checkpointer 17+ — see its own doc
// comment for the real incident this exists to surface, and why
// PostgreSQL 17's split of these statistics into a new view matters for
// a project supporting the whole [12, 18] range) actually works against
// a REAL PostgreSQL instance, whichever version happens to be running —
// rather than assuming the query syntax is right for a branch this test
// environment's own Postgres version might never exercise.
func TestFetchCheckpointPressure_UsesTheRightViewForTheRunningVersion(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()

	versionNum, _, err := db.FetchPostgresVersion(ctx, pool)
	if err != nil {
		t.Fatalf("could not determine the running PostgreSQL version: %v", err)
	}

	pressure, err := db.FetchCheckpointPressure(ctx, pool, versionNum)
	if err != nil {
		t.Fatalf("FetchCheckpointPressure failed against a real PostgreSQL %d instance: %v", versionNum, err)
	}
	if pressure.RequestedCheckpoints < 0 || pressure.TimedCheckpoints < 0 {
		t.Errorf("expected non-negative checkpoint counters, got requested=%d timed=%d",
			pressure.RequestedCheckpoints, pressure.TimedCheckpoints)
	}
}

package upgrade

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSQLiteStore_CreateAndGetJob_RoundTrip(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	job := &Job{
		Schemas:              []string{"public", "billing"},
		SourceConnectionRef:  "postgresql://source",
		TargetConnectionRef:  "postgresql://target",
		SourceReplicationRef: "postgresql://replication-host",
		Tables: []TableRef{
			{SchemaName: "reporting", TableName: "events"},
			{SchemaName: "reporting", TableName: "sessions"},
		},
	}
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected CreateJob to assign a non-empty ID")
	}

	got, err := store.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if got.SourceReplicationRef != "postgresql://replication-host" {
		t.Errorf("expected SourceReplicationRef to round-trip, got %q", got.SourceReplicationRef)
	}
	if len(got.Schemas) != 2 || got.Schemas[0] != "public" || got.Schemas[1] != "billing" {
		t.Errorf("expected Schemas to round-trip, got %v", got.Schemas)
	}
	if len(got.Tables) != 2 || got.Tables[0] != (TableRef{SchemaName: "reporting", TableName: "events"}) ||
		got.Tables[1] != (TableRef{SchemaName: "reporting", TableName: "sessions"}) {
		t.Errorf("expected Tables to round-trip, got %+v", got.Tables)
	}
}

func TestSQLiteStore_CreateJob_EmptySourceReplicationRef_DefaultsToEmptyString(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	job := &Job{SourceConnectionRef: "postgresql://source", TargetConnectionRef: "postgresql://target"}
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	got, err := store.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if got.SourceReplicationRef != "" {
		t.Errorf("expected an empty SourceReplicationRef when never set, got %q", got.SourceReplicationRef)
	}
}

// TestNewSQLiteStore_MigratesExistingDatabaseMissingNewerColumns is the
// direct regression test for the real bug found during manual GUI
// testing: a database file created by an OLDER version of this package
// (before SourceReplicationRef existed) must still work with the
// CURRENT code — CREATE TABLE IF NOT EXISTS alone does NOT add a new
// column to an already-existing table, which is exactly what
// migrateSchema exists to fix. This test manually creates the OLD
// schema (missing BOTH source_replication_ref and tables_json — every
// column added to upgrade_jobs after its original release, see
// migrateSchema's own addedColumns list) to reproduce that exact
// starting state, then confirms NewSQLiteStore migrates it in place and
// CreateJob succeeds afterward — before the original fix, the INSERT
// below would fail with "no such column: source_replication_ref"
// (surfaced to a caller as a generic 500); this test now also proves a
// SECOND, later-added column (tables_json, for Priority 3's
// table-level scoping) migrates through the exact same mechanism
// without needing its own bespoke test.
func TestNewSQLiteStore_MigratesExistingDatabaseMissingNewerColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")

	// Step 1: create the OLD schema directly via database/sql, bypassing
	// NewSQLiteStore entirely — reproducing exactly what an
	// already-existing database file from an older binary looks like.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open raw sqlite connection: %v", err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE upgrade_jobs (
			id                    TEXT PRIMARY KEY,
			phase                 TEXT NOT NULL,
			schemas               TEXT NOT NULL,
			source_connection_ref TEXT NOT NULL,
			target_connection_ref TEXT NOT NULL,
			last_error            TEXT NOT NULL DEFAULT '',
			created_at            TEXT NOT NULL,
			updated_at            TEXT NOT NULL,
			tables_total          INTEGER NOT NULL DEFAULT 0,
			tables_synced         INTEGER NOT NULL DEFAULT 0,
			tables_verified       INTEGER NOT NULL DEFAULT 0
		)
	`)
	if err != nil {
		t.Fatalf("failed to create the OLD schema: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("failed to close raw connection: %v", err)
	}

	// Step 2: open it through NewSQLiteStore, exactly as a real restart
	// of pgarchimigrator would — this is where the migration must run.
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed to open/migrate a pre-existing OLD-schema database: %v", err)
	}
	defer store.Close()

	// Step 3: the real proof — CreateJob (which INSERTs both
	// source_replication_ref AND tables_json) must now succeed against
	// what was originally a database file missing both columns
	// entirely. Tables is deliberately populated here (not left empty)
	// so this test also proves tables_json specifically survives the
	// migration and round-trips, not just that the INSERT no longer
	// errors.
	job := &Job{
		SourceConnectionRef: "postgresql://source",
		TargetConnectionRef: "postgresql://target",
		Tables:              []TableRef{{SchemaName: "public", TableName: "orders"}},
	}
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob failed against a migrated OLD-schema database: %v", err)
	}

	got, err := store.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob failed against a migrated OLD-schema database: %v", err)
	}
	if len(got.Tables) != 1 || got.Tables[0] != (TableRef{SchemaName: "public", TableName: "orders"}) {
		t.Errorf("expected Tables to round-trip through the migrated tables_json column, got %+v", got.Tables)
	}
}

// TestNewSQLiteStore_ReopeningAlreadyMigratedDatabase_DoesNotFail
// confirms the migration is safe to run repeatedly — every normal
// startup of pgarchimigrator calls NewSQLiteStore again against the
// SAME file, which must never fail just because the column is already
// present (see migrateSchema's own doc comment on
// using PRAGMA table_info specifically to make this safe).
func TestNewSQLiteStore_ReopeningAlreadyMigratedDatabase_DoesNotFail(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")

	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("first open failed: %v", err)
	}
	store1.Close()

	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("second open of the SAME already-migrated database failed: %v", err)
	}
	defer store2.Close()

	job := &Job{SourceConnectionRef: "postgresql://source", TargetConnectionRef: "postgresql://target"}
	if err := store2.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob failed after reopening: %v", err)
	}
}

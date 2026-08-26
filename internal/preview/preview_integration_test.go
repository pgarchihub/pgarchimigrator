//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/preview/... -tags=integration -v
package preview_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/preview"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
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

// tableStatsFetcher mirrors cmd/pgarchimigrator/main.go's buildWiring closure —
// the exact same conversion a real Orchestrator uses, so preview tests
// exercise the real strategy-decision path, not a stub.
func tableStatsFetcher(pool *pgxpool.Pool) orchestrator.TableStatsFetcher {
	return func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
		raw, err := db.FetchTableStats(ctx, pool, schema, table)
		if err != nil {
			return strategy.TableStats{}, err
		}
		return strategy.TableStats{
			EstimatedRowCount: raw.EstimatedRowCount,
			IsPartitioned:     raw.IsPartitioned,
			HasPrimaryKey:     raw.HasPrimaryKey,
			ReplicaIdentity:   raw.ReplicaIdentity,
		}, nil
	}
}

func createTestTable(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
		t.Fatalf("could not clean up old table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, existing_col TEXT)
	`, name)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name))
	})
}

// TestGenerate_NeverExecutesAnyDDL is the single most important property
// this whole package must have: previewing a migration must leave the
// database in EXACTLY the state it was in before, regardless of the
// operation. This test runs a preview for an operation that would
// normally add a real column, then verifies that column does NOT exist.
func TestGenerate_NeverExecutesAnyDDL(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_no_ddl_test"
	createTestTable(t, pool, tableName)

	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{Operation: strategy.OpAddColumn, ColumnName: "should_not_exist", NewType: "TEXT"},
	}

	if _, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	var colExists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = 'should_not_exist')`
	if err := pool.QueryRow(ctx, checkQuery, tableName).Scan(&colExists); err != nil {
		t.Fatalf("column check failed: %v", err)
	}
	if colExists {
		t.Fatal("dry run must never actually create the column — this is the core guarantee of this package")
	}
}

// TestGenerate_AddColumn_ReflectsRealStrategyDecision verifies the
// previewed strategy is computed via the SAME strategy.Decide() logic a
// real migration uses, not guessed or hardcoded — a small table should
// show DIRECT_DDL, matching what would actually run.
func TestGenerate_AddColumn_ReflectsRealStrategyDecision(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_strategy_test"
	createTestTable(t, pool, tableName)

	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{Operation: strategy.OpAddColumn, ColumnName: "status", NewType: "TEXT", DefaultValue: "'active'"},
	}

	report, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if report.Strategy != string(strategy.StrategyDirectDDL) {
		t.Errorf("expected Strategy=DIRECT_DDL for a small table, got %s", report.Strategy)
	}
	if len(report.Statements) == 0 {
		t.Fatal("expected at least one previewed statement")
	}
	if !strings.Contains(report.Statements[0], "ADD COLUMN") {
		t.Errorf("expected the previewed statement to mention ADD COLUMN, got: %s", report.Statements[0])
	}
}

// TestGenerate_SetNotNull_WarnsAboutExistingNulls is the core value
// proposition of this whole package: a real, read-only check against the
// actual data catching a migration that WOULD fail, before anything runs.
func TestGenerate_SetNotNull_WarnsAboutExistingNulls(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_setnotnull_warn_test"
	createTestTable(t, pool, tableName)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (existing_col) VALUES ('a'), (NULL), ('b')`, tableName)); err != nil {
		t.Fatalf("could not seed test data: %v", err)
	}

	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{Operation: strategy.OpSetNotNull, ColumnName: "existing_col"},
	}

	report, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("expected a warning about the existing NULL value, got none")
	}
	if !strings.Contains(report.Warnings[0], "1 existing row") {
		t.Errorf("expected the warning to mention exactly 1 existing NULL row, got: %s", report.Warnings[0])
	}
}

// TestGenerate_SetNotNull_NoWarningWhenDataIsClean verifies the
// symmetric case: no false-positive warning when every row already
// satisfies the constraint.
func TestGenerate_SetNotNull_NoWarningWhenDataIsClean(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_setnotnull_clean_test"
	createTestTable(t, pool, tableName)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (existing_col) VALUES ('a'), ('b')`, tableName)); err != nil {
		t.Fatalf("could not seed test data: %v", err)
	}

	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{Operation: strategy.OpSetNotNull, ColumnName: "existing_col"},
	}

	report, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if len(report.Warnings) != 0 {
		t.Errorf("expected no warnings for clean data, got: %v", report.Warnings)
	}
	if len(report.Notes) == 0 {
		t.Error("expected a reassuring note confirming validation should succeed")
	}
}

// TestGenerate_AddConstraint_WarnsAboutViolatingRows verifies the same
// kind of real, read-only pre-check for an arbitrary user-supplied CHECK
// expression, not just the built-in NOT NULL case.
func TestGenerate_AddConstraint_WarnsAboutViolatingRows(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_addconstraint_warn_test"
	createTestTable(t, pool, tableName)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (existing_col) VALUES ('abc'), (''), ('xyz')`, tableName)); err != nil {
		t.Fatalf("could not seed test data: %v", err)
	}

	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{
			Operation: strategy.OpAddConstraint, ConstraintName: "existing_col_not_empty",
			CheckExpression: "length(existing_col) > 0",
		},
	}

	report, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("expected a warning about the row violating the check expression, got none")
	}
	if !strings.Contains(report.Warnings[0], "1 existing row") {
		t.Errorf("expected the warning to mention exactly 1 violating row, got: %s", report.Warnings[0])
	}
}

// TestGenerate_DropIndex_CapturesRealDefinition verifies the DROP_INDEX
// preview reads and shows the ACTUAL current index definition from the
// database, not a guess.
func TestGenerate_DropIndex_CapturesRealDefinition(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_dropindex_test"
	createTestTable(t, pool, tableName)

	indexName := "idx_preview_dropindex_manual"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX %s ON %s (existing_col)`, indexName, tableName)); err != nil {
		t.Fatalf("could not create the index: %v", err)
	}

	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{Operation: strategy.OpDropIndex, IndexName: indexName},
	}

	report, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	found := false
	for _, note := range report.Notes {
		if strings.Contains(note, "existing_col") && strings.Contains(note, indexName) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a note containing the real captured index definition (mentioning %q and existing_col), got notes: %v", indexName, report.Notes)
	}

	// The index must still exist — this was only a preview.
	var stillExists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = $1)`
	if err := pool.QueryRow(ctx, checkQuery, indexName).Scan(&stillExists); err != nil {
		t.Fatalf("index check failed: %v", err)
	}
	if !stillExists {
		t.Error("expected the index to still exist after a dry run")
	}
}

// TestGenerate_RenameColumn_ExplainsDualWriteMechanism verifies the
// RENAME_COLUMN preview clearly explains the expand & contract mechanism
// rather than implying a plain (and misleading) rename.
//
// This deliberately does NOT assert on report.Strategy: the test table is
// empty, so strategy.Decide's small-table shortcut returns DIRECT_DDL
// regardless of operation (the same behavior every other operation in
// this file sees on an empty table) — RENAME_COLUMN's own
// large-table -> EXPAND_BACKFILL mapping is verified directly, without
// needing a real multi-million-row table, in
// internal/strategy's own unit tests instead.
func TestGenerate_RenameColumn_ExplainsDualWriteMechanism(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_rename_test"
	createTestTable(t, pool, tableName)

	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{Operation: strategy.OpRenameColumn, ColumnName: "existing_col", NewColumnName: "renamed_col"},
	}

	report, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if len(report.Notes) == 0 || !strings.Contains(report.Notes[0], "dual-write") {
		t.Errorf("expected a note explaining the dual-write mechanism, got: %v", report.Notes)
	}
}

// TestGenerate_NeverReturnsNilSlices is a regression test for a real,
// user-reported production bug: Statements/Warnings/Notes were declared
// as Go's zero-value nil slices and only ever appended to — so for any
// operation/data combination that legitimately leaves one of them with
// zero entries (e.g. SET_NOT_NULL on already-clean data never appends a
// Warning, only a Note), that field stayed nil. encoding/json marshals a
// nil slice as JSON `null`, and the frontend's `preview.Warnings.length`
// crashed on that null the instant such a preview rendered — with no
// React error boundary anywhere in the app at the time, that blanked the
// ENTIRE screen. len(x) == 0 is true for BOTH nil and an empty non-nil
// slice, so the earlier tests in this file (which only checked len())
// could never have caught this — this test explicitly asserts non-nil,
// and additionally proves it via the actual JSON encoding, which is what
// the real bug manifested through.
func TestGenerate_NeverReturnsNilSlices(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "preview_nil_slices_test"
	createTestTable(t, pool, tableName)

	// SET_NOT_NULL on a table with NO existing NULLs: this is exactly the
	// code path that appends to Notes but never touches Warnings at all —
	// the precise scenario that produced a nil Warnings field before the
	// fix.
	req := orchestrator.MigrationRequest{
		SchemaName: "public", TableName: tableName,
		Change: strategy.ColumnChange{Operation: strategy.OpSetNotNull, ColumnName: "existing_col"},
	}

	report, err := preview.Generate(ctx, pool, tableStatsFetcher(pool), req)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if report.Statements == nil {
		t.Error("expected Statements to be a non-nil (possibly empty) slice")
	}
	if report.Warnings == nil {
		t.Error("expected Warnings to be a non-nil (possibly empty) slice — this is the exact field that was nil in the real bug")
	}
	if report.Notes == nil {
		t.Error("expected Notes to be a non-nil (possibly empty) slice")
	}

	// Prove it via the real encoding path, not just the Go-level nil
	// check — this is what the frontend actually receives over the wire.
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("failed to marshal report: %v", err)
	}
	if strings.Contains(string(encoded), `"Warnings":null`) {
		t.Error("Warnings serialized as JSON null — this is exactly what crashed the frontend (null.length is not a function)")
	}
	if strings.Contains(string(encoded), `"Statements":null`) {
		t.Error("Statements serialized as JSON null")
	}
	if strings.Contains(string(encoded), `"Notes":null`) {
		t.Error("Notes serialized as JSON null")
	}
}

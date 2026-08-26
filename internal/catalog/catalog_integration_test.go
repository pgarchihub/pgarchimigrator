//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/catalog/... -tags=integration -v
package catalog_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/catalog"
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

func TestListSchemas_ExcludesSystemSchemas_IncludesPublic(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	schemas, err := catalog.ListSchemas(ctx, pool)
	if err != nil {
		t.Fatalf("ListSchemas failed: %v", err)
	}

	found := false
	for _, s := range schemas {
		if s == "public" {
			found = true
		}
		if s == "pg_catalog" || s == "information_schema" {
			t.Errorf("expected system schema %q to be excluded, but it was returned", s)
		}
	}
	if !found {
		t.Error("expected the 'public' schema to be present")
	}
}

func TestListTables_ReturnsOnlyBaseTables_ExcludesViews(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS catalog_list_tables_test CASCADE`)
	_, _ = pool.Exec(ctx, `DROP VIEW IF EXISTS catalog_list_tables_view_test`)
	if _, err := pool.Exec(ctx, `CREATE TABLE catalog_list_tables_test (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE VIEW catalog_list_tables_view_test AS SELECT id FROM catalog_list_tables_test`); err != nil {
		t.Fatalf("could not create test view: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DROP VIEW IF EXISTS catalog_list_tables_view_test`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS catalog_list_tables_test CASCADE`)
	})

	tables, err := catalog.ListTables(ctx, pool, "public")
	if err != nil {
		t.Fatalf("ListTables failed: %v", err)
	}

	var hasTable, hasView bool
	for _, tbl := range tables {
		if tbl == "catalog_list_tables_test" {
			hasTable = true
		}
		if tbl == "catalog_list_tables_view_test" {
			hasView = true
		}
	}
	if !hasTable {
		t.Error("expected the real table to be listed")
	}
	if hasView {
		t.Error("expected the view to be EXCLUDED from ListTables — it only lists BASE TABLE, not VIEW")
	}
}

func TestListTables_UnknownSchema_ReturnsEmptyNotError(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()

	tables, err := catalog.ListTables(ctx, pool, "schema_that_does_not_exist")
	if err != nil {
		t.Fatalf("expected no error for an unknown schema (just zero results), got: %v", err)
	}
	if tables == nil {
		t.Error("expected a non-nil (empty) slice, got nil — this would serialize to JSON null and crash a frontend .map() call")
	}
	if len(tables) != 0 {
		t.Errorf("expected zero tables for a nonexistent schema, got %d", len(tables))
	}
}

func TestListColumns_ReturnsNamesAndTypesInPhysicalOrder(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "catalog_list_columns_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, name VARCHAR(50), amount NUMERIC(10,2))
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	columns, err := catalog.ListColumns(ctx, pool, "public", tableName)
	if err != nil {
		t.Fatalf("ListColumns failed: %v", err)
	}
	if len(columns) != 3 {
		t.Fatalf("expected 3 columns, got %d: %+v", len(columns), columns)
	}

	// Physical (attnum) order — matches CREATE TABLE's declaration order.
	want := []catalog.ColumnInfo{
		{Name: "id", Type: "bigint", Nullable: false, IsPrimaryKey: true, Default: ""},
		{Name: "name", Type: "character varying(50)", Nullable: true, IsPrimaryKey: false, Default: ""},
		{Name: "amount", Type: "numeric(10,2)", Nullable: true, IsPrimaryKey: false, Default: ""},
	}
	for i, w := range want {
		if columns[i] != w {
			t.Errorf("column %d: expected %+v, got %+v", i, w, columns[i])
		}
	}
}

// TestListColumns_ReportsNullableDefaultAndPrimaryKey covers the
// constraint metadata added for the New Migration screen's table-overview
// panel — each column exercises a different combination so no single
// flag being wrong-but-coincidentally-passing slips through.
func TestListColumns_ReportsNullableDefaultAndPrimaryKey(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "catalog_constraints_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (
			id BIGINT PRIMARY KEY,
			status TEXT NOT NULL DEFAULT 'active',
			note TEXT
		)
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	columns, err := catalog.ListColumns(ctx, pool, "public", tableName)
	if err != nil {
		t.Fatalf("ListColumns failed: %v", err)
	}
	if len(columns) != 3 {
		t.Fatalf("expected 3 columns, got %d: %+v", len(columns), columns)
	}

	id, status, note := columns[0], columns[1], columns[2]

	if !id.IsPrimaryKey {
		t.Error("expected id to be reported as the primary key")
	}
	if id.Nullable {
		t.Error("expected id to be non-nullable (implied by PRIMARY KEY)")
	}
	if status.IsPrimaryKey {
		t.Error("expected status to NOT be reported as a primary key")
	}
	if status.Nullable {
		t.Error("expected status to be non-nullable (explicit NOT NULL)")
	}
	if status.Default != "'active'::text" {
		t.Errorf("expected status's default to be \"'active'::text\", got %q", status.Default)
	}
	if !note.Nullable {
		t.Error("expected note to be nullable (no constraint given)")
	}
	if note.Default != "" {
		t.Errorf("expected note to have no default, got %q", note.Default)
	}
}

func TestSampleRows_ReturnsUpToLimitRowsWithColumnNames(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "catalog_sample_rows_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, label TEXT)
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	for i := 1; i <= 8; i++ {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, label) VALUES (%d, 'row-%d')`, tableName, i, i)); err != nil {
			t.Fatalf("could not insert seed row: %v", err)
		}
	}

	result, err := catalog.SampleRows(ctx, pool, "public", tableName, 5)
	if err != nil {
		t.Fatalf("SampleRows failed: %v", err)
	}
	if len(result.Columns) != 2 || result.Columns[0] != "id" || result.Columns[1] != "label" {
		t.Errorf("expected columns [id label], got %v", result.Columns)
	}
	if len(result.Rows) != 5 {
		t.Fatalf("expected exactly 5 rows (the limit) out of 8 inserted, got %d", len(result.Rows))
	}
	// Every cell must already be a plain string — that's the whole point
	// of SampleRows over a raw driver-typed scan (see its doc comment).
	if result.Rows[0][0] == "" {
		t.Error("expected the id cell to be a non-empty stringified value")
	}
}

func TestSampleRows_NullCellsRenderAsNULL(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "catalog_sample_rows_null_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, note TEXT)`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, note) VALUES (1, NULL)`, tableName)); err != nil {
		t.Fatalf("could not insert seed row: %v", err)
	}

	result, err := catalog.SampleRows(ctx, pool, "public", tableName, 5)
	if err != nil {
		t.Fatalf("SampleRows failed: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0][1] != "NULL" {
		t.Errorf("expected the NULL cell to render as the string \"NULL\", got %v", result.Rows)
	}
}

// TestSampleRows_TableNameWithSpecialCharacters_DoesNotBreak is a light
// regression guard for the identifier-quoting approach itself (see
// SampleRows' doc comment on why pgx.Identifier{}.Sanitize() is used
// instead of string concatenation) — a table name that would break a
// naively-concatenated query must still work correctly here.
func TestSampleRows_TableNameWithSpecialCharacters_DoesNotBreak(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := `catalog"weird table`

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, pgxIdentifierForTest(tableName)))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY)`, pgxIdentifierForTest(tableName))); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, pgxIdentifierForTest(tableName)))
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id) VALUES (1)`, pgxIdentifierForTest(tableName))); err != nil {
		t.Fatalf("could not insert seed row: %v", err)
	}

	result, err := catalog.SampleRows(ctx, pool, "public", tableName, 5)
	if err != nil {
		t.Fatalf("SampleRows failed on a table name with a double quote in it: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(result.Rows))
	}
}

// pgxIdentifierForTest quotes a table name for use in this test file's OWN
// setup/teardown SQL (creating/dropping the awkwardly-named test table) —
// deliberately independent of catalog's internal quoting so this test
// doesn't just exercise the same code path on both sides.
func pgxIdentifierForTest(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// TestListColumns_ExcludesDroppedColumns verifies a column dropped via
// DROP_COLUMN (or a plain DROP COLUMN) doesn't leak into the list —
// PostgreSQL keeps a tombstone pg_attribute row for dropped columns
// rather than removing it outright, so this must be filtered explicitly
// (see the NOT a.attisdropped clause in the query).
func TestListColumns_ExcludesDroppedColumns(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "catalog_list_columns_dropped_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, temp_col TEXT)
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN temp_col`, tableName)); err != nil {
		t.Fatalf("could not drop the column: %v", err)
	}

	columns, err := catalog.ListColumns(ctx, pool, "public", tableName)
	if err != nil {
		t.Fatalf("ListColumns failed: %v", err)
	}
	for _, c := range columns {
		if c.Name == "temp_col" {
			t.Error("expected the dropped column to be excluded from the list")
		}
	}
	if len(columns) != 1 || columns[0].Name != "id" {
		t.Errorf("expected only the surviving 'id' column, got %+v", columns)
	}
}

//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/typecompat/... -tags=integration -v
package typecompat_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/typecompat"
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

// relfilenode identifies the physical file backing a table. A table
// rewrite (PostgreSQL creating a new physical copy) always changes it;
// a metadata-only ALTER never does — this is the definitive, empirical
// way to verify this package's "compatible" claims against what
// PostgreSQL itself actually does, not just against our own logic.
func relfilenode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, table string) uint32 {
	t.Helper()
	var oid uint32
	query := `
		SELECT pg_relation_filenode(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
	`
	if err := pool.QueryRow(ctx, query, schema, table).Scan(&oid); err != nil {
		t.Fatalf("could not read relfilenode: %v", err)
	}
	return oid
}

func TestCurrentColumnType_ReturnsExactFormattedType(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "typecompat_currenttype_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, name VARCHAR(50), amount NUMERIC(10,2))
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	nameType, err := typecompat.CurrentColumnType(ctx, pool, "public", tableName, "name")
	if err != nil {
		t.Fatalf("CurrentColumnType failed: %v", err)
	}
	if nameType != "character varying(50)" {
		t.Errorf("expected %q, got %q", "character varying(50)", nameType)
	}

	amountType, err := typecompat.CurrentColumnType(ctx, pool, "public", tableName, "amount")
	if err != nil {
		t.Fatalf("CurrentColumnType failed: %v", err)
	}
	if amountType != "numeric(10,2)" {
		t.Errorf("expected %q, got %q", "numeric(10,2)", amountType)
	}
}

// TestIsCompatible_VarcharWidening_MatchesRealPostgresNoRewrite is the
// strongest possible verification of this package's safety claims:
// widening a varchar(10) to varchar(20) — a case IsCompatible says is
// "free" — must produce NO actual table rewrite in real PostgreSQL
// (relfilenode unchanged), or this package's core claim would be false.
func TestIsCompatible_VarcharWidening_MatchesRealPostgresNoRewrite(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "typecompat_widen_norewrite_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, name VARCHAR(10))
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 'hello')`, tableName)); err != nil {
		t.Fatalf("could not seed test data: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	oldType, err := typecompat.CurrentColumnType(ctx, pool, "public", tableName, "name")
	if err != nil {
		t.Fatalf("CurrentColumnType failed: %v", err)
	}
	if !typecompat.IsCompatible(oldType, "varchar(20)") {
		t.Fatalf("expected varchar(10) -> varchar(20) to be recognized as compatible, got false for oldType=%q", oldType)
	}

	before := relfilenode(t, ctx, pool, "public", tableName)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN name TYPE varchar(20)`, tableName)); err != nil {
		t.Fatalf("ALTER COLUMN TYPE failed: %v", err)
	}

	after := relfilenode(t, ctx, pool, "public", tableName)
	if before != after {
		t.Error("expected NO table rewrite (relfilenode unchanged) for a case this package claims is compatible — the safety claim does not hold against real PostgreSQL")
	}
}

// TestIsCompatible_TextToInteger_CorrectlyRejected_RealPostgresRewrites is
// the negative control: text -> integer is correctly identified as NOT
// compatible, and — to prove that rejection is meaningful, not just
// overly cautious — real PostgreSQL genuinely DOES rewrite the table for
// this change (relfilenode changes).
func TestIsCompatible_TextToInteger_CorrectlyRejected_RealPostgresRewrites(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "typecompat_reject_rewrites_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT)
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, '100')`, tableName)); err != nil {
		t.Fatalf("could not seed test data: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	if typecompat.IsCompatible("text", "integer") {
		t.Fatal("expected text -> integer to be rejected as incompatible")
	}

	before := relfilenode(t, ctx, pool, "public", tableName)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN amount TYPE integer USING amount::integer`, tableName)); err != nil {
		t.Fatalf("ALTER COLUMN TYPE failed: %v", err)
	}

	after := relfilenode(t, ctx, pool, "public", tableName)
	if before == after {
		t.Error("expected a real table rewrite for text -> integer (relfilenode should change) — if it didn't, our rejection here is overly conservative and this case could safely be reclassified")
	}
}

// TestIsCompatible_VarcharShrinking_CorrectlyRejected verifies the other
// direction of the negative control: shrinking a varchar length must
// never be classified as compatible, since it can reject or truncate
// existing data and always needs a validating scan.
func TestIsCompatible_VarcharShrinking_CorrectlyRejected(t *testing.T) {
	oldType := "character varying(50)"
	if typecompat.IsCompatible(oldType, "varchar(10)") {
		t.Error("expected shrinking a varchar length to be rejected as incompatible")
	}
}

// TestIsCompatible_NumericWidening_MatchesRealPostgresNoRewrite mirrors
// the varchar no-rewrite verification for numeric precision widening at a
// fixed scale.
func TestIsCompatible_NumericWidening_MatchesRealPostgresNoRewrite(t *testing.T) {
	pool := connectPool(t)
	ctx := context.Background()
	tableName := "typecompat_numeric_widen_test"

	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id BIGINT PRIMARY KEY, amount NUMERIC(5,2))
	`, tableName)); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1, 123.45)`, tableName)); err != nil {
		t.Fatalf("could not seed test data: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName)) })

	oldType, err := typecompat.CurrentColumnType(ctx, pool, "public", tableName, "amount")
	if err != nil {
		t.Fatalf("CurrentColumnType failed: %v", err)
	}
	if !typecompat.IsCompatible(oldType, "numeric(10,2)") {
		t.Fatalf("expected numeric(5,2) -> numeric(10,2) to be recognized as compatible, got false for oldType=%q", oldType)
	}

	before := relfilenode(t, ctx, pool, "public", tableName)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN amount TYPE numeric(10,2)`, tableName)); err != nil {
		t.Fatalf("ALTER COLUMN TYPE failed: %v", err)
	}

	after := relfilenode(t, ctx, pool, "public", tableName)
	if before != after {
		t.Error("expected NO table rewrite for numeric precision widening at a fixed scale — the safety claim does not hold against real PostgreSQL")
	}
}

//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/shadowflow/... -tags=integration -v -run TestValidateChunkedChecksum -timeout 60s
package shadowflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests live in package shadowflow (not shadowflow_test) so they can
// call the unexported validateChunkedChecksum directly — this isolates
// checksum-specific failures from the full Execute pipeline (which, since
// its existing test tables all use an integer PK, already implicitly
// exercises this code path on every successful migration).

const checksumTestDSN = "postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable"

func connectChecksumTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), checksumTestDSN)
	if err != nil {
		t.Fatalf("could not connect (is docker compose up?): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestValidateChunkedChecksum_PassesForIdenticalData verifies the happy
// path across MULTIPLE chunks (small batch size, more rows than one
// batch), with the cast-column normalization applied.
func TestValidateChunkedChecksum_PassesForIdenticalData(t *testing.T) {
	pool := connectChecksumTestPool(t)
	ctx := context.Background()

	source := "checksum_ok_source"
	shadow := "checksum_ok_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)`, source)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount INTEGER NOT NULL)`, shadow)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, amount) SELECT g, g::text FROM generate_series(1, 250) g
	`, source)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, amount) SELECT g, g FROM generate_series(1, 250) g
	`, shadow)); err != nil {
		t.Fatalf("could not seed shadow table: %v", err)
	}

	err := validateChunkedChecksum(ctx, pool, "public", source, shadow, []string{"id"}, "amount", "integer", 100 /* 250 rows / 100 = 3 batches */)
	if err != nil {
		t.Errorf("expected identical data (modulo the documented cast) to validate cleanly, got: %v", err)
	}
}

// TestValidateChunkedChecksum_DetectsValueMismatch verifies a single
// corrupted row's value (not just a missing/extra row — row count would
// still match) is actually caught, proving this check is meaningfully
// stronger than the row-count check alone (TR-10's whole point).
func TestValidateChunkedChecksum_DetectsValueMismatch(t *testing.T) {
	pool := connectChecksumTestPool(t)
	ctx := context.Background()

	source := "checksum_bad_source"
	shadow := "checksum_bad_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount TEXT NOT NULL)`, source)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id BIGINT PRIMARY KEY, amount INTEGER NOT NULL)`, shadow)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, amount) VALUES (1, '100'), (2, '200'), (3, '300')
	`, source)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	// Row id=2's amount is deliberately wrong (should be 200) — same row
	// count as source, so ONLY a checksum check (not row-count) can catch this.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, amount) VALUES (1, 100), (2, 999), (3, 300)
	`, shadow)); err != nil {
		t.Fatalf("could not seed shadow table: %v", err)
	}

	err := validateChunkedChecksum(ctx, pool, "public", source, shadow, []string{"id"}, "amount", "integer", 10000)
	if err == nil {
		t.Fatal("expected a checksum mismatch to be detected")
	}
}

// TestValidateChunkedChecksum_TextPK_DetectsValueMismatch verifies that a
// non-integer (text) primary key is now FULLY validated via checksum —
// this used to be a documented limitation (skipped entirely), closed by
// switching pagination to PostgreSQL's native ROW/tuple comparison
// operator instead of a text-cast cursor.
func TestValidateChunkedChecksum_TextPK_DetectsValueMismatch(t *testing.T) {
	pool := connectChecksumTestPool(t)
	ctx := context.Background()

	source := "checksum_text_source"
	shadow := "checksum_text_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, amount TEXT NOT NULL)`, source)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, amount INTEGER NOT NULL)`, shadow)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES ('a', '100'), ('b', '200')`, source)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	// Row 'b's amount is deliberately wrong (should be 200).
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES ('a', 100), ('b', 999)`, shadow)); err != nil {
		t.Fatalf("could not seed shadow table: %v", err)
	}

	err := validateChunkedChecksum(ctx, pool, "public", source, shadow, []string{"id"}, "amount", "integer", 10000)
	if err == nil {
		t.Fatal("expected a checksum mismatch to be detected for a text primary key")
	}
}

// TestValidateChunkedChecksum_TextPK_PassesForIdenticalData is the
// corresponding happy path for a text PK, across multiple batches.
func TestValidateChunkedChecksum_TextPK_PassesForIdenticalData(t *testing.T) {
	pool := connectChecksumTestPool(t)
	ctx := context.Background()

	source := "checksum_text_ok_source"
	shadow := "checksum_text_ok_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, amount TEXT NOT NULL)`, source)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, amount INTEGER NOT NULL)`, shadow)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, amount) SELECT 'row-' || lpad(g::text, 4, '0'), g::text FROM generate_series(1, 250) g
	`, source)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, amount) SELECT 'row-' || lpad(g::text, 4, '0'), g FROM generate_series(1, 250) g
	`, shadow)); err != nil {
		t.Fatalf("could not seed shadow table: %v", err)
	}

	err := validateChunkedChecksum(ctx, pool, "public", source, shadow, []string{"id"}, "amount", "integer", 100 /* 3 batches */)
	if err != nil {
		t.Errorf("expected identical text-PK data to validate cleanly, got: %v", err)
	}
}

// TestValidateChunkedChecksum_CompositeKey_PassesForIdenticalData verifies
// composite primary key support end to end, across multiple batches —
// this is the feature this file was extended to add.
func TestValidateChunkedChecksum_CompositeKey_PassesForIdenticalData(t *testing.T) {
	pool := connectChecksumTestPool(t)
	ctx := context.Background()

	source := "checksum_composite_ok_source"
	shadow := "checksum_composite_ok_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (tenant_id INTEGER, id BIGINT, amount TEXT NOT NULL, PRIMARY KEY (tenant_id, id))
	`, source)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (tenant_id INTEGER, id BIGINT, amount INTEGER NOT NULL, PRIMARY KEY (tenant_id, id))
	`, shadow)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	})

	// Two tenants, 150 rows each — deliberately more than one batch, and
	// interleaved tenant IDs so tuple ordering (tenant_id, id) is actually exercised.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (tenant_id, id, amount)
		SELECT t, g, g::text FROM generate_series(1, 2) t, generate_series(1, 150) g
	`, source)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (tenant_id, id, amount)
		SELECT t, g, g FROM generate_series(1, 2) t, generate_series(1, 150) g
	`, shadow)); err != nil {
		t.Fatalf("could not seed shadow table: %v", err)
	}

	err := validateChunkedChecksum(ctx, pool, "public", source, shadow, []string{"tenant_id", "id"}, "amount", "integer", 100 /* multiple batches across 300 total rows */)
	if err != nil {
		t.Errorf("expected identical composite-key data to validate cleanly, got: %v", err)
	}
}

// TestValidateChunkedChecksum_CompositeKey_DetectsValueMismatch verifies a
// single corrupted row is caught even when it's identified by a composite
// key (same row count either way — only a checksum check can catch this).
func TestValidateChunkedChecksum_CompositeKey_DetectsValueMismatch(t *testing.T) {
	pool := connectChecksumTestPool(t)
	ctx := context.Background()

	source := "checksum_composite_bad_source"
	shadow := "checksum_composite_bad_shadow"
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (tenant_id INTEGER, id BIGINT, amount TEXT NOT NULL, PRIMARY KEY (tenant_id, id))
	`, source)); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (tenant_id INTEGER, id BIGINT, amount INTEGER NOT NULL, PRIMARY KEY (tenant_id, id))
	`, shadow)); err != nil {
		t.Fatalf("could not create shadow table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, source, shadow))
	})

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (tenant_id, id, amount) VALUES (1, 1, '100'), (1, 2, '200'), (2, 1, '300')
	`, source)); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	// (1, 2)'s amount is deliberately wrong (should be 200) — same row
	// count as source, so ONLY a checksum check catches this.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (tenant_id, id, amount) VALUES (1, 1, 100), (1, 2, 999), (2, 1, 300)
	`, shadow)); err != nil {
		t.Fatalf("could not seed shadow table: %v", err)
	}

	err := validateChunkedChecksum(ctx, pool, "public", source, shadow, []string{"tenant_id", "id"}, "amount", "integer", 10000)
	if err == nil {
		t.Fatal("expected a checksum mismatch to be detected for a composite primary key")
	}
}

// TestValidateChunkedChecksum_NoPrimaryKey_SkipsGracefully verifies the one
// remaining, genuinely unavoidable limitation: a table with NO primary key
// at all has no reliable, efficient way to page through rows for
// comparison, so checksum validation is skipped (row-count comparison,
// done separately by the caller, is the only check in that case).
func TestValidateChunkedChecksum_NoPrimaryKey_SkipsGracefully(t *testing.T) {
	pool := connectChecksumTestPool(t)
	ctx := context.Background()

	err := validateChunkedChecksum(ctx, pool, "public", "irrelevant_source", "irrelevant_shadow",
		nil, "amount", "integer", 10000)
	if err != nil {
		t.Errorf("expected a missing primary key to skip checksum validation without even querying, got: %v", err)
	}
}

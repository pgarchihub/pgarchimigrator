//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/engines/postgresql/upgrade/... -tags=integration -v -run TestValidateTable -timeout 60s
package upgrade_test

import (
	"context"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/engines/postgresql/upgrade"
)

func TestValidateTable_IdenticalData_Matches(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_validate_match`)
	_, _ = targetPool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_validate_match`)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = sourcePool.Exec(bg, `DROP TABLE IF EXISTS upgrade_validate_match`)
		_, _ = targetPool.Exec(bg, `DROP TABLE IF EXISTS upgrade_validate_match`)
	})

	const ddl = `CREATE TABLE upgrade_validate_match (id BIGINT PRIMARY KEY, note TEXT NOT NULL)`
	if _, err := sourcePool.Exec(ctx, ddl); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := targetPool.Exec(ctx, ddl); err != nil {
		t.Fatalf("could not create target table: %v", err)
	}

	const seed = `INSERT INTO upgrade_validate_match (id, note) SELECT g, 'note-' || g FROM generate_series(1, 100) g`
	if _, err := sourcePool.Exec(ctx, seed); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	if _, err := targetPool.Exec(ctx, seed); err != nil {
		t.Fatalf("could not seed target table: %v", err)
	}

	ref := upgrade.TableRef{SchemaName: "public", TableName: "upgrade_validate_match"}
	result, err := upgrade.ValidateTable(ctx, sourcePool, targetPool, ref)
	if err != nil {
		t.Fatalf("ValidateTable failed: %v", err)
	}
	if !result.Matched {
		t.Errorf("expected identical source/target data to match, got: source=(%d rows, checksum %d) target=(%d rows, checksum %d)",
			result.SourceRowCount, result.SourceChecksum, result.TargetRowCount, result.TargetChecksum)
	}
}

// TestValidateTable_DifferentRowCount_DoesNotMatch is the direct,
// deterministic proof that ValidateTable actually catches a real
// mismatch — seeds source and target with a genuinely different number
// of rows (no timing/replication involved at all, just two plain
// INSERTs), so there's nothing racy about this test unlike trying to
// inject drift into a live subscription mid-flight.
func TestValidateTable_DifferentRowCount_DoesNotMatch(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_validate_mismatch`)
	_, _ = targetPool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_validate_mismatch`)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = sourcePool.Exec(bg, `DROP TABLE IF EXISTS upgrade_validate_mismatch`)
		_, _ = targetPool.Exec(bg, `DROP TABLE IF EXISTS upgrade_validate_mismatch`)
	})

	const ddl = `CREATE TABLE upgrade_validate_mismatch (id BIGINT PRIMARY KEY)`
	if _, err := sourcePool.Exec(ctx, ddl); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := targetPool.Exec(ctx, ddl); err != nil {
		t.Fatalf("could not create target table: %v", err)
	}

	if _, err := sourcePool.Exec(ctx, `INSERT INTO upgrade_validate_mismatch (id) SELECT g FROM generate_series(1, 50) g`); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	// Deliberately fewer rows on the target — a real drift scenario.
	if _, err := targetPool.Exec(ctx, `INSERT INTO upgrade_validate_mismatch (id) SELECT g FROM generate_series(1, 40) g`); err != nil {
		t.Fatalf("could not seed target table: %v", err)
	}

	ref := upgrade.TableRef{SchemaName: "public", TableName: "upgrade_validate_mismatch"}
	result, err := upgrade.ValidateTable(ctx, sourcePool, targetPool, ref)
	if err != nil {
		t.Fatalf("ValidateTable failed: %v", err)
	}
	if result.Matched {
		t.Error("expected a row-count mismatch to be detected, but ValidateTable reported a match")
	}
	if result.SourceRowCount != 50 || result.TargetRowCount != 40 {
		t.Errorf("expected source=50/target=40, got source=%d/target=%d", result.SourceRowCount, result.TargetRowCount)
	}
}

// TestValidateTable_SameRowCountDifferentData_DoesNotMatch confirms the
// checksum half of ValidateTable's own check actually does something —
// same NUMBER of rows on both sides, but different CONTENT, must still
// be caught. A row-count-only check would incorrectly pass this case.
func TestValidateTable_SameRowCountDifferentData_DoesNotMatch(t *testing.T) {
	sourcePool := connectTestPool(t, sourceTestDSN)
	targetPool := connectTestPool(t, targetTestDSN)
	ctx := context.Background()

	_, _ = sourcePool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_validate_content_mismatch`)
	_, _ = targetPool.Exec(ctx, `DROP TABLE IF EXISTS upgrade_validate_content_mismatch`)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = sourcePool.Exec(bg, `DROP TABLE IF EXISTS upgrade_validate_content_mismatch`)
		_, _ = targetPool.Exec(bg, `DROP TABLE IF EXISTS upgrade_validate_content_mismatch`)
	})

	const ddl = `CREATE TABLE upgrade_validate_content_mismatch (id BIGINT PRIMARY KEY, note TEXT NOT NULL)`
	if _, err := sourcePool.Exec(ctx, ddl); err != nil {
		t.Fatalf("could not create source table: %v", err)
	}
	if _, err := targetPool.Exec(ctx, ddl); err != nil {
		t.Fatalf("could not create target table: %v", err)
	}

	if _, err := sourcePool.Exec(ctx, `INSERT INTO upgrade_validate_content_mismatch (id, note) VALUES (1, 'original')`); err != nil {
		t.Fatalf("could not seed source table: %v", err)
	}
	// Same row count (1 row, same id) but DIFFERENT content.
	if _, err := targetPool.Exec(ctx, `INSERT INTO upgrade_validate_content_mismatch (id, note) VALUES (1, 'DIFFERENT')`); err != nil {
		t.Fatalf("could not seed target table: %v", err)
	}

	ref := upgrade.TableRef{SchemaName: "public", TableName: "upgrade_validate_content_mismatch"}
	result, err := upgrade.ValidateTable(ctx, sourcePool, targetPool, ref)
	if err != nil {
		t.Fatalf("ValidateTable failed: %v", err)
	}
	if result.Matched {
		t.Error("expected a content mismatch (same row count, different data) to be detected, but ValidateTable reported a match")
	}
	if result.SourceRowCount != result.TargetRowCount {
		t.Errorf("expected equal row counts for this test case (that's the point), got source=%d target=%d", result.SourceRowCount, result.TargetRowCount)
	}
	if result.SourceChecksum == result.TargetChecksum {
		t.Error("expected different checksums for different content")
	}
}
